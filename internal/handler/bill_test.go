package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/OatApisit/billsplit-api/internal/model"
)

// A member must not be able to keep the settlement and drop the bill that
// justified it.
//
// The attack needs no race: Alice records a bill naming Bob as payer with
// herself as the sole participant, which makes her owe Bob a million; she
// settles exactly that, which passes the bound; then she deletes the bill, which
// she is allowed to do because she created it. The settlement survives — Bob
// cannot delete it, he did not send it — and the plan now reads Bob owing Alice
// a million. The ledger sums to zero throughout, so settle.Minimize never
// objects.
//
// Deleting the ErrSettlementDepends guard in repo.DeleteBill fails this test.
func TestDeleteBillRefusedWhileASettlementDependsOnIt(t *testing.T) {
	ta := newTestApp(t)
	group := ta.mustGroup(t, "Dinner", "U_alice", "U_bob")

	status, body := ta.do(t, "U_alice", http.MethodPost, "/api/groups/"+group+"/bills",
		fiber.Map{
			"title": "Invented", "total": "1000000.00", "mode": "exact",
			"payerId":      "U_bob",
			"participants": []string{"U_alice"},
			"shares":       []string{"1000000.00"},
		})
	if status != http.StatusCreated {
		t.Fatalf("POST bill: %d %s", status, body)
	}

	var bill model.Bill
	if err := json.Unmarshal(body, &bill); err != nil {
		t.Fatal(err)
	}

	status, body = ta.do(t, "U_alice", http.MethodPost, "/api/groups/"+group+"/settlements",
		fiber.Map{"toUser": "U_bob", "amount": "1000000.00"})
	if status != http.StatusCreated {
		t.Fatalf("POST settlement: %d %s", status, body)
	}
	if net := ta.netOf(t, "U_bob", group, "U_bob"); net != 0 {
		t.Fatalf("bob is at %s once the pair cancels out, want 0.00", net)
	}

	status, body = ta.do(t, "U_alice", http.MethodDelete,
		"/api/groups/"+group+"/bills/"+bill.ID, nil)
	if status != http.StatusConflict {
		t.Fatalf("deleting a bill a settlement depends on: %d %s, want 409", status, body)
	}

	// The whole point: Bob must not be left owing a million for a payment that
	// never happened.
	if net := ta.netOf(t, "U_bob", group, "U_bob"); net != 0 {
		t.Errorf("bob is at %s after the refused delete, want 0.00", net)
	}
	if net := ta.netOf(t, "U_alice", group, "U_alice"); net != 0 {
		t.Errorf("alice is at %s after the refused delete, want 0.00", net)
	}

	// The bill is still there — a refused delete must not half-apply.
	bills, err := ta.repo.ListBills(ta.ctx, group)
	if err != nil {
		t.Fatal(err)
	}
	if len(bills) != 1 {
		t.Errorf("got %d bills after the refused delete, want 1", len(bills))
	}

	// And the escape hatch works: once the sender withdraws the settlement, the
	// bill can be deleted. An honest member is delayed, not trapped.
	settlements, err := ta.repo.ListSettlements(ta.ctx, group)
	if err != nil {
		t.Fatal(err)
	}
	if len(settlements) != 1 {
		t.Fatalf("got %d settlements, want 1", len(settlements))
	}
	status, body = ta.do(t, "U_alice", http.MethodDelete,
		"/api/groups/"+group+"/settlements/"+settlements[0].ID, nil)
	if status != http.StatusNoContent {
		t.Fatalf("withdrawing own settlement: %d %s, want 204", status, body)
	}
	status, body = ta.do(t, "U_alice", http.MethodDelete,
		"/api/groups/"+group+"/bills/"+bill.ID, nil)
	if status != http.StatusNoContent {
		t.Fatalf("deleting the bill once nothing depends on it: %d %s, want 204", status, body)
	}
}

// The guard must not become a blanket ban on deletion. A bill with no settlement
// leaning on it is still freely withdrawable, and so is one whose group holds
// only settlements between other members that it never justified.
func TestDeleteBillStillWorksWhenNoSettlementDependsOnIt(t *testing.T) {
	ta := newTestApp(t)
	group := ta.mustGroup(t, "Trip", "U_alice", "U_bob", "U_cat", "U_dan")

	// Alice's bill: Alice pays 200.00 split with Bob. Nobody settles it.
	status, body := ta.do(t, "U_alice", http.MethodPost, "/api/groups/"+group+"/bills",
		fiber.Map{
			"title": "Taxi", "total": "200.00", "mode": "equal",
			"participants": []string{"U_alice", "U_bob"},
		})
	if status != http.StatusCreated {
		t.Fatalf("POST alice's bill: %d %s", status, body)
	}
	var alicesBill model.Bill
	if err := json.Unmarshal(body, &alicesBill); err != nil {
		t.Fatal(err)
	}

	// Cat's bill, which Dan settles. This settlement has nothing to do with
	// Alice's bill and must not freeze it.
	status, body = ta.do(t, "U_cat", http.MethodPost, "/api/groups/"+group+"/bills",
		fiber.Map{
			"title": "Hotel", "total": "200.00", "mode": "equal",
			"participants": []string{"U_cat", "U_dan"},
		})
	if status != http.StatusCreated {
		t.Fatalf("POST cat's bill: %d %s", status, body)
	}
	status, body = ta.do(t, "U_dan", http.MethodPost, "/api/groups/"+group+"/settlements",
		fiber.Map{"toUser": "U_cat", "amount": "100.00"})
	if status != http.StatusCreated {
		t.Fatalf("POST dan's settlement: %d %s", status, body)
	}

	status, body = ta.do(t, "U_alice", http.MethodDelete,
		"/api/groups/"+group+"/bills/"+alicesBill.ID, nil)
	if status != http.StatusNoContent {
		t.Fatalf("deleting an unsettled bill: %d %s, want 204", status, body)
	}

	// Dan's payment is untouched and both of them are square.
	if net := ta.netOf(t, "U_cat", group, "U_cat"); net != 0 {
		t.Errorf("cat is at %s, want 0.00", net)
	}
	if net := ta.netOf(t, "U_bob", group, "U_bob"); net != 0 {
		t.Errorf("bob is at %s once the bill against him is gone, want 0.00", net)
	}
}
