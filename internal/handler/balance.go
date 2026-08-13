package handler

import (
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/OatApisit/billsplit-api/internal/line"
	"github.com/OatApisit/billsplit-api/internal/model"
	"github.com/OatApisit/billsplit-api/internal/money"
	"github.com/OatApisit/billsplit-api/internal/settle"
)

// balanceEntry is one member's position, with the name attached so the client
// does not have to join against the member list to render it.
type balanceEntry struct {
	User model.User   `json:"user"`
	Paid money.Satang `json:"paid"`
	Owed money.Satang `json:"owed"`
	Net  money.Satang `json:"net"`
}

type balancesResponse struct {
	Balances  []balanceEntry    `json:"balances"`
	Transfers []settle.Transfer `json:"transfers"`
}

// balances reports where the group stands and the shortest way to clear it.
func (h *Handler) balances(c *fiber.Ctx) error {
	groupID, _, err := h.requireMember(c)
	if err != nil {
		return err
	}

	b, err := h.computeBalances(c, groupID)
	if err != nil {
		return err
	}
	return c.JSON(balancesResponse{Balances: b.entries, Transfers: b.transfers})
}

// groupBalances is everything the balance computation produces: the per-member
// rows the client renders, the transfer plan, and the member lookup the chat
// summary needs for names.
//
// The net positions are not here. The settlement bound needs them read inside
// the transaction that inserts, which is a ledger this function never sees; see
// netsOf.
type groupBalances struct {
	entries   []balanceEntry
	transfers []settle.Transfer
	byID      map[string]model.User
}

// computeBalances is the shared body behind the balances endpoint and the chat
// summary: both need the same numbers, and computing them in two places is how
// the API and the message people actually read drift apart.
func (h *Handler) computeBalances(c *fiber.Ctx, groupID string) (*groupBalances, error) {
	ledger, err := h.repo.Ledger(c.UserContext(), groupID)
	if err != nil {
		return nil, err
	}
	members, err := h.repo.Members(c.UserContext(), groupID)
	if err != nil {
		return nil, err
	}

	byID := make(map[string]model.User, len(members))
	for _, m := range members {
		byID[m.ID] = m
	}

	paid, owed := ledgerTotals(ledger)
	balances := settle.Net(paid, owed)
	transfers, err := settle.Minimize(balances)
	if err != nil {
		// Minimize only fails on a ledger that does not sum to zero, which
		// would mean the stored bills and shares disagree — a data integrity
		// problem, not a bad request.
		return nil, err
	}

	entries := make([]balanceEntry, 0, len(balances))
	for _, b := range balances {
		entries = append(entries, balanceEntry{
			User: byID[b.UserID],
			Paid: paid[b.UserID],
			Owed: owed[b.UserID],
			Net:  b.Net,
		})
	}
	return &groupBalances{entries: entries, transfers: transfers, byID: byID}, nil
}

// ledgerTotals splits the ledger rows into the two maps settle.Net takes.
func ledgerTotals(ledger []model.Ledger) (paid, owed map[string]money.Satang) {
	paid = make(map[string]money.Satang, len(ledger))
	owed = make(map[string]money.Satang, len(ledger))
	for _, l := range ledger {
		paid[l.UserID] = l.Paid
		owed[l.UserID] = l.Owed
	}
	return paid, owed
}

// netsOf reduces a ledger to net positions, which is all the settlement bound
// needs — no member names, no transfer plan, nothing that would require a second
// query inside the transaction holding the group lock.
func netsOf(ledger []model.Ledger) map[string]money.Satang {
	balances := settle.Net(ledgerTotals(ledger))
	nets := make(map[string]money.Satang, len(balances))
	for _, b := range balances {
		nets[b.UserID] = b.Net
	}
	return nets
}

type createSettlementRequest struct {
	ToUser string `json:"toUser"`
	Amount string `json:"amount"`
	Note   string `json:"note"`
}

func (h *Handler) listSettlements(c *fiber.Ctx) error {
	groupID, _, err := h.requireMember(c)
	if err != nil {
		return err
	}

	settlements, err := h.repo.ListSettlements(c.UserContext(), groupID)
	if err != nil {
		return err
	}
	return c.JSON(settlements)
}

// createSettlement records that the caller paid another member.
//
// Only the sender may record their own payment. Letting anyone file a
// settlement on someone else's behalf would let a member erase their own debt
// by claiming a transfer that never happened.
func (h *Handler) createSettlement(c *fiber.Ctx) error {
	groupID, userID, err := h.requireMember(c)
	if err != nil {
		return err
	}

	var req createSettlementRequest
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "malformed body")
	}

	amount, err := money.ParseBaht(req.Amount)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "amount is not a valid amount")
	}
	if amount <= 0 {
		return fiber.NewError(fiber.StatusBadRequest, "amount must be greater than zero")
	}
	if req.ToUser == userID {
		return fiber.NewError(fiber.StatusBadRequest, "cannot settle with yourself")
	}
	if err := h.assertMembers(c, groupID, []string{req.ToUser}); err != nil {
		return err
	}

	// A settlement is a claim that shrinks the sender's own debt, so it is
	// bounded by the debt: without this, any member can post an arbitrary
	// amount and drive another member's balance to nonsense that no endpoint
	// can undo except by deleting the settlement again.
	//
	// The bound is handed to the repo rather than checked here first, so that the
	// ledger it reads is the one visible inside the inserting transaction, under
	// the group lock. Checked out here, it would be a snapshot from before the
	// lock, and a concurrent bill deletion could remove the debt this settlement
	// was admitted against.
	settlement, err := h.repo.CreateSettlement(c.UserContext(), model.Settlement{
		GroupID:  groupID,
		FromUser: userID,
		ToUser:   req.ToUser,
		Amount:   amount,
		Note:     strings.TrimSpace(req.Note),
	}, func(ledger []model.Ledger) error {
		limit := outstandingTo(netsOf(ledger), userID, req.ToUser)
		if amount > limit {
			return fiber.NewError(fiber.StatusBadRequest,
				"amount exceeds the "+limit.String()+" that can be settled between you and this member")
		}
		return nil
	})
	if err != nil {
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(settlement)
}

// outstandingTo reports the largest payment from may record to to: the smaller
// of what from owes the group and what the group owes to.
//
// The net positions are the bound, not the transfer plan. The plan is one of
// several ways to flatten the same nets, and money that actually changed hands
// does not consult it: with nets A -100, B +100, C -100, D +100 the plan pairs
// A→B and C→D, but if C handed cash to B instead, that payment is real and the
// group is no worse off for recording it. Bounding by the plan would tell C they
// owe nothing — which is false, C owes 100.00, just not to B under this plan —
// and leave a real payment permanently unrecordable.
//
// The pair bound is exactly as safe as the plan bound because neither party can
// cross zero: the sender cannot pay out more than they owe, and the recipient
// cannot be credited more than they are owed. That invariant is what
// repo.DeleteBill then protects from being unwound after the fact.
func outstandingTo(nets map[string]money.Satang, from, to string) money.Satang {
	debt := -nets[from]
	credit := nets[to]

	// A member who is owed money has no debt to settle, and one who owes money
	// has no credit to receive; either way the bound is zero, not a negative.
	if debt < 0 {
		debt = 0
	}
	if credit < 0 {
		credit = 0
	}
	return min(debt, credit)
}

// deleteSettlement withdraws a payment the caller recorded.
//
// Only the sender may withdraw their own, and anything else — another member's
// settlement, another group's, an ID that never existed — is a 404. A recorded
// payment is a claim, and a claim someone else can retract is as dangerous as
// one nobody can.
func (h *Handler) deleteSettlement(c *fiber.Ctx) error {
	groupID, userID, err := h.requireMember(c)
	if err != nil {
		return err
	}

	if err := h.repo.DeleteSettlement(c.UserContext(), groupID,
		c.Params("settlementId"), userID); err != nil {
		return notFoundAsHTTP(err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// pushSummary posts the settlement summary back into the LINE chat the group
// is bound to.
func (h *Handler) pushSummary(c *fiber.Ctx) error {
	groupID, userID, err := h.requireMember(c)
	if err != nil {
		return err
	}
	if h.pusher == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable,
			"messaging is not configured on this server")
	}

	group, err := h.repo.GetGroup(c.UserContext(), groupID, userID)
	if err != nil {
		return notFoundAsHTTP(err)
	}
	if group.LineGroupID == "" {
		return fiber.NewError(fiber.StatusBadRequest,
			"this group is not linked to a LINE chat")
	}

	// A group with no bills has nothing to summarise, and the summary of one is
	// close to the payload a phishing attempt wants: the group name, which the
	// caller chose, delivered by the official bot into a chat, with no figures
	// to contradict it.
	//
	// This gate does NOT close that path. It raises its cost by exactly one
	// request — an attacker posts a 0.01 bill against themselves and the push
	// proceeds, now carrying one line of real-looking figures. The actual fix is
	// proving the caller is in the chat they bound the group to, which needs the
	// Messaging API and therefore the webhook this project has not built yet;
	// see "Not built yet" in CLAUDE.md. The gate is kept because it costs a real
	// group nothing — there is no moment where a member wants to announce an
	// empty ledger — not because it is a defence.
	hasBills, err := h.repo.HasBills(c.UserContext(), groupID)
	if err != nil {
		return err
	}
	if !hasBills {
		return fiber.NewError(fiber.StatusBadRequest,
			"this group has no bills to summarise yet")
	}

	balances, err := h.computeBalances(c, groupID)
	if err != nil {
		return err
	}

	lines := make([]line.SettlementLine, 0, len(balances.transfers))
	for _, t := range balances.transfers {
		lines = append(lines, line.SettlementLine{
			FromName: displayName(balances.byID, t.From),
			ToName:   displayName(balances.byID, t.To),
			Amount:   t.Amount,
		})
	}

	flex := line.SettlementFlex(group.Name, lines, h.cfg.LiffURL)
	if err := h.pusher.Push(c.UserContext(), group.LineGroupID, flex); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"pushed": len(lines)})
}

func displayName(byID map[string]model.User, userID string) string {
	if u, ok := byID[userID]; ok && u.DisplayName != "" {
		return u.DisplayName
	}
	return "สมาชิก"
}
