package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/OatApisit/billsplit-api/internal/model"
	"github.com/OatApisit/billsplit-api/internal/money"
)

// The bound on a settlement is the smaller of what the sender owes the group
// and what the group owes the recipient — neither party may cross zero. The
// reported attack — owing 100.00 and posting 1,000,000,000.00 to drive the
// victim to -999,999,800.00 — is the "wildly over" case here.
func TestOutstandingTo(t *testing.T) {
	// Bob owes 100.00 and Cat owes 33.33; Alice is owed 133.33.
	nets := map[string]money.Satang{
		"U_alice": 13333,
		"U_bob":   -10000,
		"U_cat":   -3333,
	}

	cases := []struct {
		name     string
		from, to string
		want     money.Satang
	}{
		{"debtor to creditor, bounded by the debt", "U_bob", "U_alice", 10000},
		{"odd satang survives", "U_cat", "U_alice", 3333},
		{"direction is not symmetric", "U_alice", "U_bob", 0},
		{"a creditor cannot pay anyone", "U_alice", "U_cat", 0},
		{"two debtors cannot settle with each other", "U_bob", "U_cat", 0},
		{"a member with no position at all", "U_ghost", "U_alice", 0},
		{"paying a member with no position", "U_bob", "U_ghost", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := outstandingTo(nets, tc.from, tc.to); got != tc.want {
				t.Errorf("outstandingTo(%s -> %s) = %s, want %s",
					tc.from, tc.to, got, tc.want)
			}
		})
	}
}

// The bound is the pair's own position, not the transfer plan. With these nets
// the plan pairs A→B and C→D, but if C actually handed cash to B then that
// payment happened, and bounding by the plan would tell C they owe 0.00 to a
// member they can legitimately pay — and leave the payment unrecordable forever.
func TestOutstandingToAllowsAPaymentTheTransferPlanDidNotPair(t *testing.T) {
	nets := map[string]money.Satang{
		"U_a": -10000,
		"U_b": 10000,
		"U_c": -10000,
		"U_d": 10000,
	}

	if got := outstandingTo(nets, "U_c", "U_b"); got != 10000 {
		t.Errorf("C -> B is bounded at %s, want 100.00: C owes 100.00 and B is owed 100.00", got)
	}
	// The creditor's own credit still caps it: C owes 100.00 in total, so C may
	// not hand B more than the 100.00 B is owed even though C owes that much.
	nets["U_b"] = 4000
	nets["U_d"] = 16000
	if got := outstandingTo(nets, "U_c", "U_b"); got != 4000 {
		t.Errorf("C -> B is bounded at %s, want 40.00: B is only owed 40.00", got)
	}
}

// Settling the exact outstanding amount is the ordinary case and must not be
// rejected by an off-by-one in the comparison — including when the amount
// carries the odd satang from a three-way split.
func TestOutstandingToAllowsExactSettlement(t *testing.T) {
	nets := map[string]money.Satang{"U_bob": -3334, "U_alice": 3334}
	owed := outstandingTo(nets, "U_bob", "U_alice")

	if exact := money.Satang(3334); exact > owed {
		t.Errorf("settling exactly %s would be rejected against an outstanding %s", exact, owed)
	}
	if under := money.Satang(3333); under > owed {
		t.Errorf("settling %s would be rejected against an outstanding %s", under, owed)
	}
	if over := money.Satang(3335); over <= owed {
		t.Errorf("settling %s would be accepted against an outstanding %s", over, owed)
	}
	if fabricated := money.Satang(100_000_000_000); fabricated <= owed {
		t.Errorf("a fabricated %s would be accepted against an outstanding %s",
			fabricated, owed)
	}
}

// A settled group leaves everyone at zero, so nothing more may be settled.
func TestOutstandingToOnASettledGroup(t *testing.T) {
	nets := map[string]money.Satang{"U_bob": 0, "U_alice": 0}
	if got := outstandingTo(nets, "U_bob", "U_alice"); got != 0 {
		t.Errorf("outstanding on a settled group = %s, want 0.00", got)
	}
}

// A settlement is a claim that shrinks the sender's own debt. Posting one for
// more than that debt is how a member drives someone else's balance to nonsense,
// so createSettlement must refuse it — not merely be able to compute the bound.
//
// Deleting the `if amount > limit` block in createSettlement fails this test.
func TestCreateSettlementRejectsMoreThanIsOwed(t *testing.T) {
	ta := newTestApp(t)
	group := ta.mustGroup(t, "Lunch", "U_alice", "U_bob")

	// Alice pays 200.00, split evenly: Bob owes her 100.00.
	status, body := ta.do(t, "U_alice", http.MethodPost, "/api/groups/"+group+"/bills",
		fiber.Map{
			"title": "Lunch", "total": "200.00", "mode": "equal",
			"participants": []string{"U_alice", "U_bob"},
		})
	if status != http.StatusCreated {
		t.Fatalf("POST bill: %d %s", status, body)
	}

	over := []struct {
		name, amount string
	}{
		{"one satang over", "100.01"},
		{"a fabricated amount", "1000000000.00"},
	}
	for _, tc := range over {
		t.Run(tc.name, func(t *testing.T) {
			status, body := ta.do(t, "U_bob", http.MethodPost, "/api/groups/"+group+"/settlements",
				fiber.Map{"toUser": "U_alice", "amount": tc.amount})
			if status != http.StatusBadRequest {
				t.Fatalf("settling %s against a 100.00 debt: %d %s, want 400",
					tc.amount, status, body)
			}
		})
	}

	// Alice is still owed exactly what the bill says, and nothing was recorded.
	if net := ta.netOf(t, "U_alice", group, "U_alice"); net != 10000 {
		t.Errorf("alice is at %s after the rejected settlements, want 100.00", net)
	}

	// The honest payment still goes through, so the guard is a bound and not a
	// blanket refusal.
	status, body = ta.do(t, "U_bob", http.MethodPost, "/api/groups/"+group+"/settlements",
		fiber.Map{"toUser": "U_alice", "amount": "100.00"})
	if status != http.StatusCreated {
		t.Fatalf("settling exactly what is owed: %d %s, want 201", status, body)
	}
	if net := ta.netOf(t, "U_alice", group, "U_alice"); net != 0 {
		t.Errorf("alice is at %s after being paid in full, want 0.00", net)
	}
}

// pushSummary has two guards of its own, and until this test they were mounted
// nowhere: the group must be linked to a LINE chat, and it must have at least
// one bill.
//
// The second is the interesting one. A summary of an empty group is close to the
// payload a phishing attempt wants — the caller's own group name, delivered by
// the official bot, with no figures to contradict it — and while the gate does
// not close that path (see the comment on pushSummary), a deleted gate should
// not be free either. Deleting either check fails this test.
func TestPushSummaryRefusesAnUnlinkedOrEmptyGroup(t *testing.T) {
	ta := newTestApp(t)
	ta.mustUser(t, "U_alice")
	ta.mustUser(t, "U_bob")

	unlinked := ta.mustGroup(t, "Browser opened", "U_alice", "U_bob")
	status, body := ta.do(t, "U_alice", http.MethodPost, "/api/groups/"+unlinked+"/summary", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("summarising a group bound to no chat: %d %s, want 400", status, body)
	}

	status, body = ta.do(t, "U_alice", http.MethodPost, "/api/groups",
		fiber.Map{"name": "Dinner", "lineGroupId": "C1234567890abcdef1234567890abcdef"})
	if status != http.StatusCreated {
		t.Fatalf("POST /groups: %d %s, want 201", status, body)
	}
	var group model.Group
	if err := json.Unmarshal(body, &group); err != nil {
		t.Fatal(err)
	}

	status, body = ta.do(t, "U_alice", http.MethodPost, "/api/groups/"+group.ID+"/summary", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("summarising a group with no bills: %d %s, want 400", status, body)
	}

	// A real group is not blocked: one bill is all it takes.
	status, body = ta.do(t, "U_alice", http.MethodPost, "/api/groups/"+group.ID+"/bills",
		fiber.Map{
			"title": "Dinner", "total": "100.00", "mode": "equal",
			"participants": []string{"U_alice"},
		})
	if status != http.StatusCreated {
		t.Fatalf("POST bill: %d %s", status, body)
	}
	status, body = ta.do(t, "U_alice", http.MethodPost, "/api/groups/"+group.ID+"/summary", nil)
	if status != http.StatusOK {
		t.Fatalf("summarising a linked group with a bill: %d %s, want 200", status, body)
	}
}

// The list routes are read paths, and their whole defence is requireMember: a
// non-member must not be able to read a group's bills or its recorded payments,
// and must not be able to tell that group from one that does not exist.
func TestListRoutesAreClosedToNonMembers(t *testing.T) {
	ta := newTestApp(t)
	group := ta.mustGroup(t, "Dinner", "U_alice")
	ta.mustUser(t, "U_stranger")

	for _, path := range []string{"/bills", "/settlements"} {
		t.Run(path, func(t *testing.T) {
			status, body := ta.do(t, "U_stranger", http.MethodGet, "/api/groups/"+group+path, nil)
			if status != http.StatusNotFound {
				t.Fatalf("a non-member reading %s: %d %s, want 404", path, status, body)
			}
			if status, body = ta.do(t, "U_alice", http.MethodGet, "/api/groups/"+group+path, nil); status != http.StatusOK {
				t.Fatalf("a member reading %s: %d %s, want 200", path, status, body)
			}
		})
	}
}
