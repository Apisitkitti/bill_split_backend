package handler

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/OatApisit/billsplit-api/internal/middleware"
	"github.com/OatApisit/billsplit-api/internal/model"
	"github.com/OatApisit/billsplit-api/internal/money"
	"github.com/OatApisit/billsplit-api/internal/repo"
)

// Split modes accepted by createBill.
const (
	splitEqual  = "equal"
	splitWeight = "weight"
	splitExact  = "exact"
)

// createBillRequest is a new expense.
//
// Amounts arrive as strings, not numbers. JSON numbers are IEEE 754 doubles in
// every browser, so a total of 1234.55 sent as a number can arrive as
// 1234.5499999999999 and lose a satang before the server ever sees it. A
// string survives the trip intact and is parsed once, on this side.
type createBillRequest struct {
	Title string `json:"title"`
	Total string `json:"total"`
	Note  string `json:"note"`

	// PayerID defaults to the caller — the common case is recording a bill you
	// just paid.
	PayerID string `json:"payerId"`

	// Mode is "equal", "weight", or "exact".
	Mode string `json:"mode"`

	// Participants are the users the bill is split between, in the order the
	// odd satang should be handed out for "equal".
	Participants []string `json:"participants"`

	// Weights pairs with Participants when Mode is "weight".
	Weights []int64 `json:"weights"`

	// Shares pairs with Participants when Mode is "exact", as baht strings.
	Shares []string `json:"shares"`
}

func (h *Handler) listBills(c *fiber.Ctx) error {
	groupID, _, err := h.requireMember(c)
	if err != nil {
		return err
	}

	bills, err := h.repo.ListBills(c.UserContext(), groupID)
	if err != nil {
		return err
	}
	return c.JSON(bills)
}

func (h *Handler) createBill(c *fiber.Ctx) error {
	groupID, userID, err := h.requireMember(c)
	if err != nil {
		return err
	}

	var req createBillRequest
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "malformed body")
	}

	req.Title = strings.TrimSpace(req.Title)
	if req.Title == "" {
		return fiber.NewError(fiber.StatusBadRequest, "title is required")
	}

	total, err := money.ParseBaht(req.Total)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "total is not a valid amount")
	}
	if total <= 0 {
		return fiber.NewError(fiber.StatusBadRequest, "total must be greater than zero")
	}

	if req.PayerID == "" {
		req.PayerID = userID
	}
	if len(req.Participants) == 0 {
		return fiber.NewError(fiber.StatusBadRequest, "participants are required")
	}

	// Everyone touched by the bill must belong to the group, payer included.
	// Without this check a caller could name any LINE user ID and quietly
	// attach a debt to a stranger, or credit one.
	if err := h.assertMembers(c, groupID, append(req.Participants, req.PayerID)); err != nil {
		return err
	}

	shares, err := splitShares(req, total)
	if err != nil {
		return err
	}

	bill, err := h.repo.CreateBill(c.UserContext(), model.Bill{
		GroupID:   groupID,
		PayerID:   req.PayerID,
		Title:     req.Title,
		Total:     total,
		Note:      strings.TrimSpace(req.Note),
		CreatedBy: middleware.CurrentUser(c).ID,
		Shares:    shares,
	})
	if err != nil {
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(bill)
}

// deleteBill withdraws a bill the caller entered.
//
// Reversibility is the defence here rather than a cap on the total: a bill can
// legitimately be large, but "exact" mode lets one member attach any share to
// another member, and until now nothing could take it back. Only the author may
// delete, and everything else is a 404 — a member must not be able to erase an
// expense somebody else recorded against them.
//
// Reversibility is not symmetric, though, and that is what the repo guard
// closes. One member can own both sides of a self-cancelling pair: post a bill
// naming another member as payer, settle the debt it invents, then delete the
// bill. The settlement survives — its sender is the attacker, so the victim
// cannot retract it — and the victim is left owing money for a payment that
// never happened. So a bill may not be deleted while a settlement still leans
// on it, and that comes back as 409 rather than 404: the caller does own this
// bill, and telling them so is what makes the fix (withdraw the settlement
// first) discoverable.
func (h *Handler) deleteBill(c *fiber.Ctx) error {
	groupID, userID, err := h.requireMember(c)
	if err != nil {
		return err
	}

	if err := h.repo.DeleteBill(c.UserContext(), groupID, c.Params("billId"), userID); err != nil {
		if errors.Is(err, repo.ErrSettlementDepends) {
			return fiber.NewError(fiber.StatusConflict,
				"a recorded payment depends on this bill; whoever sent that payment must withdraw it first")
		}
		return notFoundAsHTTP(err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// splitShares turns a request into per-person amounts that sum to the total.
func splitShares(req createBillRequest, total money.Satang) ([]model.Share, error) {
	n := len(req.Participants)

	var amounts []money.Satang
	switch req.Mode {
	case "", splitEqual:
		var err error
		if amounts, err = money.SplitEqual(total, n); err != nil {
			return nil, fiber.NewError(fiber.StatusBadRequest, err.Error())
		}

	case splitWeight:
		if len(req.Weights) != n {
			return nil, fiber.NewError(fiber.StatusBadRequest,
				"weights must have one entry per participant")
		}
		var err error
		if amounts, err = money.SplitByWeight(total, req.Weights); err != nil {
			return nil, fiber.NewError(fiber.StatusBadRequest, err.Error())
		}

	case splitExact:
		if len(req.Shares) != n {
			return nil, fiber.NewError(fiber.StatusBadRequest,
				"shares must have one entry per participant")
		}
		amounts = make([]money.Satang, n)
		for i, raw := range req.Shares {
			amount, err := money.ParseBaht(raw)
			if err != nil {
				return nil, fiber.NewError(fiber.StatusBadRequest,
					fmt.Sprintf("share %d is not a valid amount", i+1))
			}
			amounts[i] = amount
		}
		// Hand-entered shares are the one path where the numbers can fail to
		// add up, so this is where the ledger is protected.
		if err := money.ValidateShares(total, amounts); err != nil {
			return nil, fiber.NewError(fiber.StatusBadRequest, err.Error())
		}

	default:
		return nil, fiber.NewError(fiber.StatusBadRequest,
			"mode must be equal, weight, or exact")
	}

	// Duplicate participants would collapse into one row on insert and lose
	// their share, silently shrinking the bill.
	seen := make(map[string]struct{}, n)
	shares := make([]model.Share, 0, n)
	for i, id := range req.Participants {
		if _, dup := seen[id]; dup {
			return nil, fiber.NewError(fiber.StatusBadRequest,
				"participant listed twice: "+id)
		}
		seen[id] = struct{}{}
		shares = append(shares, model.Share{UserID: id, Amount: amounts[i]})
	}
	return shares, nil
}

// assertMembers rejects the request unless every named user is in the group.
func (h *Handler) assertMembers(c *fiber.Ctx, groupID string, userIDs []string) error {
	members, err := h.repo.Members(c.UserContext(), groupID)
	if err != nil {
		return err
	}

	set := make(map[string]struct{}, len(members))
	for _, m := range members {
		set[m.ID] = struct{}{}
	}
	for _, id := range userIDs {
		if _, ok := set[id]; !ok {
			return fiber.NewError(fiber.StatusBadRequest, "not a member of this group: "+id)
		}
	}
	return nil
}
