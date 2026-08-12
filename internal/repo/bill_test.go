package repo

import (
	"errors"
	"testing"

	"github.com/OatApisit/billsplit-api/internal/model"
)

// A bill can be withdrawn only by the member who entered it. That is the whole
// defence against "exact" mode being used to attach an invented debt to
// somebody else: the amount may legitimately be large, so reversibility, not a
// cap, is what makes it survivable.
func TestDeleteBill(t *testing.T) {
	r, ctx := newTestRepo(t)

	alice := mustUser(t, r, ctx, "U_alice", "Alice")
	bob := mustUser(t, r, ctx, "U_bob", "Bob")
	stranger := mustUser(t, r, ctx, "U_stranger", "Stranger")

	group, err := r.CreateGroup(ctx, "Dinner", alice.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.AddMember(ctx, group.ID, bob.ID); err != nil {
		t.Fatal(err)
	}

	// Bob attaches the whole bill to Alice — the reported attack shape.
	bill, err := r.CreateBill(ctx, model.Bill{
		GroupID: group.ID, PayerID: bob.ID, CreatedBy: bob.ID,
		Title: "Invented", Total: 100_000_000_000,
		Shares: []model.Share{{UserID: alice.ID, Amount: 100_000_000_000}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// The victim cannot delete it, and learns nothing about it either.
	if err := r.DeleteBill(ctx, group.ID, bill.ID, alice.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("victim deleting another member's bill: got %v, want ErrNotFound", err)
	}
	if err := r.DeleteBill(ctx, group.ID, bill.ID, stranger.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("stranger deleting a bill: got %v, want ErrNotFound", err)
	}

	otherGroup, err := r.CreateGroup(ctx, "Other", bob.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteBill(ctx, otherGroup.ID, bill.ID, bob.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting a bill through the wrong group: got %v, want ErrNotFound", err)
	}

	// The author can, and the shares go with it — a bill whose shares outlived
	// it would leave the debt in place with nothing to explain it.
	if err := r.DeleteBill(ctx, group.ID, bill.ID, bob.ID); err != nil {
		t.Fatalf("author cannot delete own bill: %v", err)
	}

	bills, err := r.ListBills(ctx, group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(bills) != 0 {
		t.Errorf("got %d bills after deleting the only one", len(bills))
	}

	ledger, err := r.Ledger(ctx, group.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range ledger {
		if l.Paid != 0 || l.Owed != 0 {
			t.Errorf("%s is at paid=%s owed=%s after the bill was deleted",
				l.UserID, l.Paid, l.Owed)
		}
	}

	// Deleting twice is a miss, not a second success.
	if err := r.DeleteBill(ctx, group.ID, bill.ID, bob.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: got %v, want ErrNotFound", err)
	}
}

// A group with no bills cannot be summarised into a chat: that push is the
// payload shape a phishing attempt wants, and no real group needs it.
func TestHasBills(t *testing.T) {
	r, ctx := newTestRepo(t)

	alice := mustUser(t, r, ctx, "U_alice", "Alice")
	group, err := r.CreateGroup(ctx, "Fresh", alice.ID, "")
	if err != nil {
		t.Fatal(err)
	}

	has, err := r.HasBills(ctx, group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Error("a brand new group reported bills")
	}

	bill, err := r.CreateBill(ctx, model.Bill{
		GroupID: group.ID, PayerID: alice.ID, CreatedBy: alice.ID,
		Title: "Dinner", Total: 10000,
		Shares: []model.Share{{UserID: alice.ID, Amount: 10000}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if has, err = r.HasBills(ctx, group.ID); err != nil || !has {
		t.Errorf("HasBills after one bill = %v, %v; want true, nil", has, err)
	}

	// Deleting the last bill takes the group back to un-summarisable.
	if err := r.DeleteBill(ctx, group.ID, bill.ID, alice.ID); err != nil {
		t.Fatal(err)
	}
	if has, err = r.HasBills(ctx, group.ID); err != nil || has {
		t.Errorf("HasBills after deleting the only bill = %v, %v; want false, nil", has, err)
	}
}
