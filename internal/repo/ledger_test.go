package repo

import (
	"errors"
	"testing"

	"github.com/OatApisit/billsplit-api/internal/model"
	"github.com/OatApisit/billsplit-api/internal/money"
	"github.com/OatApisit/billsplit-api/internal/settle"
)

// Dinner for three: one person pays, and the ledger must show them 2/3 up and
// the others each 1/3 down, with the odd satang accounted for.
func TestLedgerAfterOneBill(t *testing.T) {
	r, ctx := newTestRepo(t)

	alice := mustUser(t, r, ctx, "U_alice", "Alice")
	bob := mustUser(t, r, ctx, "U_bob", "Bob")
	cat := mustUser(t, r, ctx, "U_cat", "Cat")

	group, err := r.CreateGroup(ctx, "Dinner", alice.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []model.User{bob, cat} {
		if err := r.AddMember(ctx, group.ID, u.ID); err != nil {
			t.Fatal(err)
		}
	}

	// 100.00 baht three ways: 33.34 / 33.33 / 33.33.
	total := money.Satang(10000)
	shares, err := money.SplitEqual(total, 3)
	if err != nil {
		t.Fatal(err)
	}

	_, err = r.CreateBill(ctx, model.Bill{
		GroupID: group.ID, PayerID: alice.ID, CreatedBy: alice.ID,
		Title: "Dinner", Total: total,
		Shares: []model.Share{
			{UserID: alice.ID, Amount: shares[0]},
			{UserID: bob.ID, Amount: shares[1]},
			{UserID: cat.ID, Amount: shares[2]},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	ledger, err := r.Ledger(ctx, group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger) != 3 {
		t.Fatalf("got %d ledger rows, want 3", len(ledger))
	}

	paid := map[string]money.Satang{}
	owed := map[string]money.Satang{}
	for _, l := range ledger {
		paid[l.UserID], owed[l.UserID] = l.Paid, l.Owed
	}

	if paid[alice.ID] != total {
		t.Errorf("alice paid %s, want %s", paid[alice.ID], total)
	}
	if owed[alice.ID] != shares[0] {
		t.Errorf("alice owes %s, want %s", owed[alice.ID], shares[0])
	}
	if paid[bob.ID] != 0 {
		t.Errorf("bob paid %s, want 0", paid[bob.ID])
	}

	// Whatever the split, the group's books must balance.
	balances := settle.Net(paid, owed)
	transfers, err := settle.Minimize(balances)
	if err != nil {
		t.Fatalf("ledger does not balance: %v", err)
	}
	if len(transfers) != 2 {
		t.Errorf("got %d transfers, want 2: %+v", len(transfers), transfers)
	}
	for _, tr := range transfers {
		if tr.To != alice.ID {
			t.Errorf("transfer to %s, want alice", tr.To)
		}
	}
}

// Recording a payment must move the ledger, not just sit in a table: a
// settlement that does not affect balances is the bug this test exists for.
func TestLedgerCountsSettlements(t *testing.T) {
	r, ctx := newTestRepo(t)

	alice := mustUser(t, r, ctx, "U_alice", "Alice")
	bob := mustUser(t, r, ctx, "U_bob", "Bob")

	group, err := r.CreateGroup(ctx, "Lunch", alice.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.AddMember(ctx, group.ID, bob.ID); err != nil {
		t.Fatal(err)
	}

	// Alice pays 200, split evenly: Bob owes her 100.
	_, err = r.CreateBill(ctx, model.Bill{
		GroupID: group.ID, PayerID: alice.ID, CreatedBy: alice.ID,
		Title: "Lunch", Total: 20000,
		Shares: []model.Share{
			{UserID: alice.ID, Amount: 10000},
			{UserID: bob.ID, Amount: 10000},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := r.CreateSettlement(ctx, model.Settlement{
		GroupID: group.ID, FromUser: bob.ID, ToUser: alice.ID, Amount: 10000,
	}); err != nil {
		t.Fatal(err)
	}

	ledger, err := r.Ledger(ctx, group.ID)
	if err != nil {
		t.Fatal(err)
	}

	paid := map[string]money.Satang{}
	owed := map[string]money.Satang{}
	for _, l := range ledger {
		paid[l.UserID], owed[l.UserID] = l.Paid, l.Owed
	}

	for _, b := range settle.Net(paid, owed) {
		if b.Net != 0 {
			t.Errorf("%s is at %s after settling, want 0", b.UserID, b.Net)
		}
	}

	transfers, err := settle.Minimize(settle.Net(paid, owed))
	if err != nil {
		t.Fatal(err)
	}
	if len(transfers) != 0 {
		t.Errorf("got %d transfers for a settled group: %+v", len(transfers), transfers)
	}
}

// A member who has neither paid nor owed anything still belongs on the roster.
func TestLedgerIncludesInactiveMembers(t *testing.T) {
	r, ctx := newTestRepo(t)

	alice := mustUser(t, r, ctx, "U_alice", "Alice")
	ghost := mustUser(t, r, ctx, "U_ghost", "Ghost")

	group, err := r.CreateGroup(ctx, "Trip", alice.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.AddMember(ctx, group.ID, ghost.ID); err != nil {
		t.Fatal(err)
	}

	ledger, err := r.Ledger(ctx, group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger) != 2 {
		t.Fatalf("got %d ledger rows, want 2", len(ledger))
	}
	for _, l := range ledger {
		if l.Paid != 0 || l.Owed != 0 {
			t.Errorf("%s starts at paid=%s owed=%s, want zero", l.UserID, l.Paid, l.Owed)
		}
	}
}

// money.MaxSatang bounds one parsed amount; nothing bounds their sum. Around
// 90,072 max-value entries carry a group's total past 2^53, where a JSON number
// stops being exact and the client renders a figure the server never sent.
// Ledger must refuse rather than serve it.
func TestCheckAggregateBoundary(t *testing.T) {
	const max = maxAggregateSatang

	cases := []struct {
		name       string
		paid, owed int64
		wantErr    bool
	}{
		{"an ordinary group", 1_000_00, 500_00, false},
		{"zero", 0, 0, false},
		{"one below the bound", max - 1, 0, false},
		{"exactly at the bound", max, max, false},
		{"one past the bound, on paid", max + 1, 0, true},
		{"one past the bound, on owed", 0, max + 1, true},
		{"far past the bound", 1 << 60, 1 << 60, true},
		{"past the bound negative", -(max + 1), 0, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkAggregate("U_alice", tc.paid, tc.owed)
			if tc.wantErr {
				if !errors.Is(err, ErrLedgerOverflow) {
					t.Fatalf("paid=%d owed=%d: got %v, want ErrLedgerOverflow",
						tc.paid, tc.owed, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("paid=%d owed=%d: rejected a representable total: %v",
					tc.paid, tc.owed, err)
			}
		})
	}
}

// The bound is the point past which a float64 can no longer hold every integer,
// so it must be 2^53 exactly and not a round decimal near it.
func TestMaxAggregateIsTheFloat64ExactLimit(t *testing.T) {
	if maxAggregateSatang != 9_007_199_254_740_992 {
		t.Errorf("bound is %d, want 2^53 = 9007199254740992", maxAggregateSatang)
	}
	if float64(maxAggregateSatang)+1 != float64(maxAggregateSatang) {
		t.Error("2^53+1 is representable as a float64; the bound is in the wrong place")
	}
}
