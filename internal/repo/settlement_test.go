package repo

import (
	"errors"
	"testing"

	"github.com/OatApisit/billsplit-api/internal/model"
	"github.com/OatApisit/billsplit-api/internal/money"
	"github.com/OatApisit/billsplit-api/internal/settle"
)

// A settlement is filed by its sender, so the sender is its author and the only
// person who may withdraw it.
func TestDeleteSettlement(t *testing.T) {
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

	s, err := r.CreateSettlement(ctx, model.Settlement{
		GroupID: group.ID, FromUser: bob.ID, ToUser: alice.ID, Amount: 10000,
	}, unbounded)
	if err != nil {
		t.Fatal(err)
	}

	// The recipient must not be able to retract a payment made to them.
	if err := r.DeleteSettlement(ctx, group.ID, s.ID, alice.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("recipient deleting the sender's settlement: got %v, want ErrNotFound", err)
	}

	if err := r.DeleteSettlement(ctx, group.ID, s.ID, bob.ID); err != nil {
		t.Fatalf("sender cannot delete own settlement: %v", err)
	}

	settlements, err := r.ListSettlements(ctx, group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(settlements) != 0 {
		t.Errorf("got %d settlements after deleting the only one", len(settlements))
	}

	// Removing the settlement must restore the debt it cleared, not leave the
	// ledger somewhere in between.
	ledger, err := r.Ledger(ctx, group.ID)
	if err != nil {
		t.Fatal(err)
	}
	paid := map[string]money.Satang{}
	owed := map[string]money.Satang{}
	for _, l := range ledger {
		paid[l.UserID], owed[l.UserID] = l.Paid, l.Owed
	}
	transfers, err := settle.Minimize(settle.Net(paid, owed))
	if err != nil {
		t.Fatal(err)
	}
	if len(transfers) != 1 || transfers[0].From != bob.ID || transfers[0].Amount != 10000 {
		t.Errorf("after undoing the settlement, transfers are %+v; want bob owing alice 100.00",
			transfers)
	}

	if err := r.DeleteSettlement(ctx, group.ID, s.ID, bob.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: got %v, want ErrNotFound", err)
	}
}
