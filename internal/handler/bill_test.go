package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
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

// The same theft, fired as two parallel requests instead of two sequential ones.
//
// The guard above is a check inside DeleteBill's transaction, and a check is
// only worth what the locking behind it is worth. Postgres does not serialise
// these two on its own: the DELETE takes a lock on the bills row, the settlement
// INSERT takes one on a settlements row that did not exist yet, and under READ
// COMMITTED neither transaction sees the other's uncommitted work. So the EXISTS
// check found no settlement, the settlement's bound still saw the debt the bill
// invented, and both committed — bill gone, settlement standing, Bob at -1000.00
// for a payment nobody made. That was not a rare interleaving; it was the usual
// outcome, and it is free to retry until it happens.
//
// Both transactions now take the same group-scoped advisory lock as their first
// statement, which leaves only two orders: the delete commits first and the
// settlement is refused for exceeding a debt that no longer exists, or the
// settlement commits first and the delete is refused with a 409. Either way the
// ledger stays at zero. Removing lockGroup from either repo.DeleteBill or
// repo.CreateSettlement fails this test.
func TestConcurrentSettlementAndBillDeleteCannotBothWin(t *testing.T) {
	ta := newTestApp(t)

	// Repeated because a race that is lost once proves nothing. With the lock
	// removed this fails within the first four attempts every time it is run;
	// this many makes a survivor a real result rather than luck.
	//
	// Which side wins is not asserted, and in practice the delete usually does:
	// createSettlement does more work before it opens its transaction. The
	// settlement-first ordering is what the sequential test above covers.
	const attempts = 40

	for i := range attempts {
		group := ta.mustGroup(t, fmt.Sprintf("Race %d", i), "U_alice", "U_bob")

		// Alice invents a bill naming Bob as payer with herself as the only
		// participant: she owes Bob 1000, and Bob is owed 1000 by nobody real.
		status, body := ta.do(t, "U_alice", http.MethodPost, "/api/groups/"+group+"/bills",
			fiber.Map{
				"title": "Invented", "total": "1000.00", "mode": "exact",
				"payerId":      "U_bob",
				"participants": []string{"U_alice"},
				"shares":       []string{"1000.00"},
			})
		if status != http.StatusCreated {
			t.Fatalf("attempt %d: POST bill: %d %s", i, status, body)
		}
		var bill model.Bill
		if err := json.Unmarshal(body, &bill); err != nil {
			t.Fatal(err)
		}

		// Both requests are held at the same gate and released together, so
		// they are in flight at once rather than one after the other.
		var (
			wg                        sync.WaitGroup
			start                     = make(chan struct{})
			settleCode, deleteCode    int
			settleBody, deleteBodyOut []byte
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			settleCode, settleBody = ta.do(t, "U_alice", http.MethodPost,
				"/api/groups/"+group+"/settlements",
				fiber.Map{"toUser": "U_bob", "amount": "1000.00"})
		}()
		go func() {
			defer wg.Done()
			<-start
			deleteCode, deleteBodyOut = ta.do(t, "U_alice", http.MethodDelete,
				"/api/groups/"+group+"/bills/"+bill.ID, nil)
		}()
		close(start)
		wg.Wait()

		settled := settleCode == http.StatusCreated
		deleted := deleteCode == http.StatusNoContent
		if settled == deleted {
			t.Fatalf("attempt %d: settle=%d %s delete=%d %s — exactly one of the two must win",
				i, settleCode, settleBody, deleteCode, deleteBodyOut)
		}

		bills, err := ta.repo.ListBills(ta.ctx, group)
		if err != nil {
			t.Fatal(err)
		}
		settlements, err := ta.repo.ListSettlements(ta.ctx, group)
		if err != nil {
			t.Fatal(err)
		}

		switch {
		case settled:
			// The settlement went first, so the bill it paid must still stand.
			//
			// Do not read this branch as coverage of that ordering: instrumented
			// over 400 attempts it ran once or twice, because createSettlement
			// does more work before it opens its transaction and the delete
			// almost always gets the lock first. What actually covers a
			// settlement-then-delete is the sequential test at the top of this
			// file, and the late-bill test below. This branch is here so that
			// the rare run is checked rather than skipped.
			if deleteCode != http.StatusConflict {
				t.Fatalf("attempt %d: settlement won but delete returned %d %s, want 409",
					i, deleteCode, deleteBodyOut)
			}
			if len(bills) != 1 || len(settlements) != 1 {
				t.Fatalf("attempt %d: settlement won with %d bills and %d settlements, want 1 and 1",
					i, len(bills), len(settlements))
			}
		case deleted:
			// The bill went first, so the debt behind the settlement was gone
			// by the time its bound was read.
			if settleCode != http.StatusBadRequest {
				t.Fatalf("attempt %d: delete won but settle returned %d %s, want 400",
					i, settleCode, settleBody)
			}
			if len(bills) != 0 || len(settlements) != 0 {
				t.Fatalf("attempt %d: delete won with %d bills and %d settlements, want 0 and 0",
					i, len(bills), len(settlements))
			}
		}

		// Whoever won, nobody is left holding a debt for a payment that did not
		// happen. This is the assertion the unlocked code failed.
		for _, u := range []string{"U_alice", "U_bob"} {
			if net := ta.netOf(t, u, group, u); net != 0 {
				t.Fatalf("attempt %d: %s is at %s (settle=%d delete=%d), want 0.00",
					i, u, net, settleCode, deleteCode)
			}
		}
	}
}

// A settlement admitted against a bill that committed while it waited must
// freeze that bill.
//
// The test above cannot see this, because both requests leave the same gate: the
// settlement's transaction never begins meaningfully before the bill exists.
// Here the bill is created while a settlement is already parked on the group
// lock, which is the ordering that broke the guard:
//
//  1. the settlement transaction BEGINs and blocks in lockGroup;
//  2. a bill commits — CreateBill takes no lock, so nothing stops it;
//  3. the settlement gets the lock, reads a ledger that now holds that bill, and
//     is admitted against it;
//  4. under DEFAULT now() it is stamped with its BEGIN time, from before the
//     bill existed, so the delete guard reads it as older and lets the bill go.
//
// The bill is then gone with the settlement it justified still standing, and its
// recipient has no endpoint that undoes a settlement they did not send.
// DEFAULT clock_timestamp() is what closes it: the row is stamped at insert,
// which is necessarily after the ledger read that admitted it. Reverting
// settlements.created_at in migration 0002 to now() fails this test — that is
// the side the stamp has to be late on, and bills.created_at is moved with it
// only because one rule stamped two ways is a rule nobody can check.
//
// No privileged database session is needed to park a settlement — a burst of
// concurrent settlement POSTs queues on the group lock on its own.
//
// What the burst cannot control is where the bill lands, so the group opens
// owing nothing at all: every settlement fired before the bill commits is
// refused by the bound for exceeding the 0.00 that can be settled. An accepted
// settlement is therefore proof that this one read a ledger already holding the
// late bill — which is the property the guard's timestamp has to agree with.
// Attempts where the bill won outright, and every settlement was refused, prove
// nothing and are retried on a fresh group.
//
// Sizing the opening debt instead — five settlements' worth, so that a sixth
// acceptance implies the late bill — does not work: the burst may simply have
// begun after the bill committed, and that run passes with now() too. It was
// written that way first and the mutant survived it.
func TestSettlementAdmittedAgainstALateBillFreezesIt(t *testing.T) {
	ta := newTestApp(t)

	const (
		// Whether an admitted settlement began before the bill is a coin toss
		// per attempt, so one attempt is not a test. Reverted to now(), eleven
		// measured runs failed on attempts ranging from 3 to 33; this is that
		// worst case with room to spare, and it costs about half a second.
		attempts = 100
		// Enough concurrent settlements to keep the lock queue non-empty for
		// the length of a bill insert, without exhausting the pool.
		burst = 8
		// The bill invents enough debt for the whole burst, so nothing is
		// refused once it is visible and the count is not capped by the bound.
		billTotal = "800.00"
		perSettle = "100.00"
		// One reproduction could be luck in the other direction — a run where
		// every admitted settlement happened to begin after the bill anyway.
		wantReproductions = 3
	)

	seen := 0
	for i := range attempts {
		group := ta.mustGroup(t, fmt.Sprintf("Late bill %d", i), "U_alice", "U_bob")

		var (
			wg       sync.WaitGroup
			start    = make(chan struct{})
			codes    = make([]int, burst)
			billCode int
			billBody []byte
		)
		wg.Add(burst + 1)
		for k := range burst {
			go func() {
				defer wg.Done()
				<-start
				codes[k], _ = ta.do(t, "U_alice", http.MethodPost,
					"/api/groups/"+group+"/settlements",
					fiber.Map{"toUser": "U_bob", "amount": perSettle})
			}()
		}
		// Alice invents a bill naming Bob as payer with herself as the only
		// participant: it is the only thing that can make any of the burst
		// admissible, and it is created while the burst is already queued.
		go func() {
			defer wg.Done()
			<-start
			billCode, billBody = ta.do(t, "U_alice", http.MethodPost,
				"/api/groups/"+group+"/bills", fiber.Map{
					"title": "Late", "total": billTotal, "mode": "exact",
					"payerId":      "U_bob",
					"participants": []string{"U_alice"},
					"shares":       []string{billTotal},
				})
		}()
		close(start)
		wg.Wait()

		if billCode != http.StatusCreated {
			t.Fatalf("attempt %d: POST late bill: %d %s", i, billCode, billBody)
		}
		var late model.Bill
		if err := json.Unmarshal(billBody, &late); err != nil {
			t.Fatal(err)
		}

		admitted := 0
		for _, code := range codes {
			if code == http.StatusCreated {
				admitted++
			}
		}
		if admitted == 0 {
			// The bill committed after the whole burst had been refused, so
			// nothing here was admitted against it and this attempt says
			// nothing. Try again on a fresh group.
			continue
		}

		// Every admitted settlement was admitted against the late bill, so
		// withdrawing it would strand a payment that has already been made.
		status, body := ta.do(t, "U_alice", http.MethodDelete,
			"/api/groups/"+group+"/bills/"+late.ID, nil)
		if status != http.StatusConflict {
			t.Fatalf("attempt %d: deleting a bill %d settlements were admitted against: %d %s, want 409",
				i, admitted, status, body)
		}

		// Bob paid nothing and only received Alice's transfers, so here he can
		// only ever be owed money. Negative is the unrecoverable state this is
		// all about: it means Alice's payments outlived the bill that justified
		// them, and Bob has no endpoint that undoes a settlement he did not
		// send. Had the delete gone through he would sit at minus everything she
		// sent.
		if net := ta.netOf(t, "U_bob", group, "U_bob"); net < 0 {
			t.Fatalf("attempt %d: bob is at %s after %d settlements and a refused delete, want no worse than 0.00",
				i, net, admitted)
		}
		seen++
	}

	// Every attempt is run rather than stopping at the first few reproductions:
	// which admitted settlements began before the bill varies run to run, and
	// with the stamp reverted to now() it is the unlucky attempt that catches it.
	if seen < wantReproductions {
		t.Fatalf("only %d of %d attempts admitted a settlement against the concurrently created bill, want at least %d; the burst is no longer parking on the lock and this test is not testing anything",
			seen, attempts, wantReproductions)
	}
	t.Logf("reproduced the late-bill interleaving on %d of %d attempts", seen, attempts)
}

// The same theft, laundered through a second, entirely genuine transfer.
//
// This is what broke the previous guard, which asked whether any member would be
// left net-positive *and* a net sender of settlements. Both halves are true here,
// but of different people: after the delete Alice is +1000 while her settlements
// net to zero, because Cat's honest repayment cancelled her outgoing one to Bob.
// Cat is a net sender but sits at zero. Bob is negative. No single member matches
// both conditions, so that guard saw nothing and Bob was left owing 1000 for a
// payment nobody made — with no endpoint he could use to undo it.
//
// Under the ordering rule every settlement here is younger than the invented
// bill, so the bill is frozen. Deleting the ErrSettlementDepends guard in
// repo.DeleteBill fails this test.
func TestDeleteBillRefusedWhenAnUnrelatedSettlementCancelsTheAttackersOwn(t *testing.T) {
	ta := newTestApp(t)
	group := ta.mustGroup(t, "Trip", "U_alice", "U_bob", "U_cat")

	// 1. Alice invents a bill naming Bob as payer and herself as sole
	//    participant: she now owes Bob 1000.
	status, body := ta.do(t, "U_alice", http.MethodPost, "/api/groups/"+group+"/bills",
		fiber.Map{
			"title": "Invented", "total": "1000.00", "mode": "exact",
			"payerId":      "U_bob",
			"participants": []string{"U_alice"},
			"shares":       []string{"1000.00"},
		})
	if status != http.StatusCreated {
		t.Fatalf("POST invented bill: %d %s", status, body)
	}
	var invented model.Bill
	if err := json.Unmarshal(body, &invented); err != nil {
		t.Fatal(err)
	}

	// 2. Alice settles it. Passes the bound: min(1000, 1000).
	status, body = ta.do(t, "U_alice", http.MethodPost, "/api/groups/"+group+"/settlements",
		fiber.Map{"toUser": "U_bob", "amount": "1000.00"})
	if status != http.StatusCreated {
		t.Fatalf("POST alice's settlement: %d %s", status, body)
	}

	// 3. An ordinary bill: Alice pays 1000.00 for Cat. Nothing suspicious.
	status, body = ta.do(t, "U_alice", http.MethodPost, "/api/groups/"+group+"/bills",
		fiber.Map{
			"title": "Hotel", "total": "1000.00", "mode": "exact",
			"participants": []string{"U_cat"},
			"shares":       []string{"1000.00"},
		})
	if status != http.StatusCreated {
		t.Fatalf("POST ordinary bill: %d %s", status, body)
	}

	// 4. Cat pays Alice back. Genuine, and passes the same bound.
	status, body = ta.do(t, "U_cat", http.MethodPost, "/api/groups/"+group+"/settlements",
		fiber.Map{"toUser": "U_alice", "amount": "1000.00"})
	if status != http.StatusCreated {
		t.Fatalf("POST cat's settlement: %d %s", status, body)
	}

	for _, u := range []string{"U_alice", "U_bob", "U_cat"} {
		if net := ta.netOf(t, u, group, u); net != 0 {
			t.Fatalf("%s is at %s before the delete, want 0.00", u, net)
		}
	}

	// 5. Alice retracts the invented bill. This is the step that must fail.
	status, body = ta.do(t, "U_alice", http.MethodDelete,
		"/api/groups/"+group+"/bills/"+invented.ID, nil)
	if status != http.StatusConflict {
		t.Fatalf("deleting the invented bill: %d %s, want 409", status, body)
	}

	for _, u := range []string{"U_alice", "U_bob", "U_cat"} {
		if net := ta.netOf(t, u, group, u); net != 0 {
			t.Errorf("%s is at %s after the refused delete, want 0.00", u, net)
		}
	}

	bills, err := ta.repo.ListBills(ta.ctx, group)
	if err != nil {
		t.Fatal(err)
	}
	if len(bills) != 2 {
		t.Errorf("got %d bills after the refused delete, want 2", len(bills))
	}
}

// The guard must not become a blanket ban on deletion, or it passes every attack
// test above by refusing everything and the product stops working.
//
// A settlement recorded *before* the bill cannot have been justified by it, so it
// is safe to leave behind and must not freeze it. Here Dan pays Cat first;
// Alice's bill is entered afterwards and is still freely withdrawable.
func TestDeleteBillStillWorksWhenTheOnlySettlementPredatesIt(t *testing.T) {
	ta := newTestApp(t)
	group := ta.mustGroup(t, "Trip", "U_alice", "U_bob", "U_cat", "U_dan")

	// Cat's bill, which Dan settles. Both land before Alice's bill exists.
	status, body := ta.do(t, "U_cat", http.MethodPost, "/api/groups/"+group+"/bills",
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

	// Alice's bill: Alice pays 200.00 split with Bob. Nobody settles it.
	status, body = ta.do(t, "U_alice", http.MethodPost, "/api/groups/"+group+"/bills",
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

	status, body = ta.do(t, "U_alice", http.MethodDelete,
		"/api/groups/"+group+"/bills/"+alicesBill.ID, nil)
	if status != http.StatusNoContent {
		t.Fatalf("deleting a bill older than nothing in the group: %d %s, want 204", status, body)
	}

	// Dan's payment is untouched and both of them are square.
	if net := ta.netOf(t, "U_cat", group, "U_cat"); net != 0 {
		t.Errorf("cat is at %s, want 0.00", net)
	}
	if net := ta.netOf(t, "U_bob", group, "U_bob"); net != 0 {
		t.Errorf("bob is at %s once the bill against him is gone, want 0.00", net)
	}
}
