package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/OatApisit/billsplit-api/internal/model"
)

// THE SPELLING TESTS — everything here varies how a request writes the group ID
// rather than what it asks for.
//
// This is the gap that let five review rounds pass a broken lock: every other
// race test in this package builds both of its URLs from the id string the
// create response returned, so both racers always agree on the spelling and the
// two of them always hash to the same advisory lock key. The lock was keyed on
// the URL text; the WHERE clauses were keyed on the uuid value; and a uuid has
// several spellings that are one value. A test that never varies the spelling
// cannot see the difference.

// upper spells a group ID the way an attacker would to get a second lock for the
// same group. Postgres reads it as the same uuid, so every WHERE clause and
// requireMember still resolve to the same group.
func upper(groupID string) string { return strings.ToUpper(groupID) }

// braced and unhyphenated are the other two spellings uuid.Parse accepts, kept
// here so the consistency test below covers the whole set rather than the one
// that happened to be reported.
func braced(groupID string) string       { return "{" + groupID + "}" }
func unhyphenated(groupID string) string { return strings.ReplaceAll(groupID, "-", "") }

// Exploit (a): the bill-delete theft, with the two requests naming the group in
// two different spellings.
//
// TestConcurrentSettlementAndBillDeleteCannotBothWin next door is the same
// attack with one spelling, and it passes against a lock keyed on the URL text.
// Change one of the two URLs to uppercase and the guard evaporates:
// hashtextextended('A0EE…') and hashtextextended('a0ee…') are different keys, so
// the delete and the settlement take different locks, block on nothing, and both
// commit — bill gone, settlement standing, Bob at -1000.00 for a payment nobody
// made, and no endpoint of his undoes it. Two reviewers reproduced that against
// a live database in 29 of 40 attempts.
//
// Reverting the canonicalisation in groupIDParam fails this test.
func TestConcurrentSettlementAndBillDeleteAtDifferentSpellingsCannotBothWin(t *testing.T) {
	ta := newTestApp(t)

	// The same count as the single-spelling race next door, for the same reason:
	// a race lost once proves nothing, and the reported reproduction rate is
	// roughly three attempts in four.
	const attempts = 40

	for i := range attempts {
		group := ta.mustGroup(t, fmt.Sprintf("Spelling race %d", i), "U_alice", "U_bob")

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

		var (
			wg                        sync.WaitGroup
			start                     = make(chan struct{})
			settleCode, deleteCode    int
			settleBody, deleteBodyOut []byte
		)
		wg.Add(2)
		// The settlement uses the canonical id the create response returned...
		go func() {
			defer wg.Done()
			<-start
			settleCode, settleBody = ta.do(t, "U_alice", http.MethodPost,
				"/api/groups/"+group+"/settlements",
				fiber.Map{"toUser": "U_bob", "amount": "1000.00"})
		}()
		// ...and the delete names the same group in upper case. Nothing else
		// differs from the single-spelling race.
		go func() {
			defer wg.Done()
			<-start
			deleteCode, deleteBodyOut = ta.do(t, "U_alice", http.MethodDelete,
				"/api/groups/"+upper(group)+"/bills/"+bill.ID, nil)
		}()
		close(start)
		wg.Wait()

		settled := settleCode == http.StatusCreated
		deleted := deleteCode == http.StatusNoContent
		if settled == deleted {
			t.Fatalf("attempt %d: settle=%d %s delete(UPPERCASE)=%d %s — exactly one of the two must win",
				i, settleCode, settleBody, deleteCode, deleteBodyOut)
		}

		// The delete named the group in a spelling of its own, so it must still
		// have hit the same rows: a 404 here would mean the uppercase path
		// resolved to no group at all and the test proved nothing about locking.
		if deleteCode == http.StatusNotFound {
			t.Fatalf("attempt %d: the uppercase path is a 404, so this test is not exercising the race",
				i)
		}

		// Whoever won, nobody is left holding a debt for a payment that did not
		// happen. This is the assertion the two-lock code failed.
		for _, u := range []string{"U_alice", "U_bob"} {
			if net := ta.netOf(t, u, group, u); net != 0 {
				t.Fatalf("attempt %d: %s is at %s (settle=%d delete=%d), want 0.00",
					i, u, net, settleCode, deleteCode)
			}
		}
	}
}

// Exploit (b): double-settling one debt, which needs no invented bill at all.
//
// This primitive did not exist before the bound moved under the group lock, and
// a lock keyed on the URL text handed it straight back. Bob genuinely owes
// 100.00. He fires two settlements of 100.00 at once, one lowercase and one
// uppercase; they take different advisory lock keys, each reads a ledger where
// the other has not committed, each is admitted against the same single debt,
// and Alice — the creditor, who was owed 100.00 — ends at -100.00 with no
// endpoint that withdraws a settlement she did not send. Reproduced against a
// live database in 39 of 40 attempts.
//
// Reverting the canonicalisation in groupIDParam fails this test.
func TestConcurrentSettlementsAtDifferentSpellingsCannotBothBeAdmitted(t *testing.T) {
	ta := newTestApp(t)

	const attempts = 40

	for i := range attempts {
		group := ta.mustGroup(t, fmt.Sprintf("Double settle %d", i), "U_alice", "U_bob")

		// An ordinary bill: Alice pays 200.00 split evenly, so Bob owes exactly
		// 100.00 and the group holds room for one settlement of that size.
		status, body := ta.do(t, "U_alice", http.MethodPost, "/api/groups/"+group+"/bills",
			fiber.Map{
				"title": "Lunch", "total": "200.00", "mode": "equal",
				"participants": []string{"U_alice", "U_bob"},
			})
		if status != http.StatusCreated {
			t.Fatalf("attempt %d: POST bill: %d %s", i, status, body)
		}

		var (
			wg     sync.WaitGroup
			start  = make(chan struct{})
			codes  [2]int
			bodies [2][]byte
			paths  = [2]string{group, upper(group)}
		)
		wg.Add(2)
		for k := range paths {
			go func() {
				defer wg.Done()
				<-start
				codes[k], bodies[k] = ta.do(t, "U_bob", http.MethodPost,
					"/api/groups/"+paths[k]+"/settlements",
					fiber.Map{"toUser": "U_alice", "amount": "100.00"})
			}()
		}
		close(start)
		wg.Wait()

		admitted := 0
		for _, code := range codes {
			if code == http.StatusCreated {
				admitted++
			}
			if code == http.StatusNotFound {
				t.Fatalf("attempt %d: a settlement path is a 404 (%d %s, %d %s), so this test is not exercising the race",
					i, codes[0], bodies[0], codes[1], bodies[1])
			}
		}
		if admitted != 1 {
			t.Fatalf("attempt %d: %d of 2 settlements admitted against a single 100.00 debt (lowercase=%d %s, uppercase=%d %s), want exactly 1",
				i, admitted, codes[0], bodies[0], codes[1], bodies[1])
		}

		// The ledger is the point: the creditor must not have become the debtor.
		if net := ta.netOf(t, "U_alice", group, "U_alice"); net != 0 {
			t.Fatalf("attempt %d: alice is at %s after being paid once for a 100.00 debt, want 0.00",
				i, net)
		}
		if net := ta.netOf(t, "U_bob", group, "U_bob"); net != 0 {
			t.Fatalf("attempt %d: bob is at %s after settling his 100.00 debt once, want 0.00", i, net)
		}

		settlements, err := ta.repo.ListSettlements(ta.ctx, group)
		if err != nil {
			t.Fatal(err)
		}
		if len(settlements) != 1 {
			t.Fatalf("attempt %d: %d settlements recorded, want 1", i, len(settlements))
		}
	}
}

// Whatever a spelling does, it must do the same thing everywhere.
//
// The decision this asserts: every spelling uuid.Parse accepts is accepted, and
// canonicalised at the handler boundary before anything uses it. The danger is
// not that an uppercase path is a 200 — it is that it is a 200 whose lock is a
// different key from the lock the lowercase path takes, which is exactly the
// state this branch shipped. So the read must succeed *and* the writes underneath
// it must serialise, which the two race tests above cover.
//
// Membership is checked on the canonical value too, so an alternate spelling is
// not a way past requireMember: a stranger gets the same 404 whichever way they
// write the ID.
func TestGroupIDSpellingsAreAcceptedConsistently(t *testing.T) {
	ta := newTestApp(t)
	group := ta.mustGroup(t, "Dinner", "U_alice", "U_bob")
	ta.mustUser(t, "U_stranger")

	status, body := ta.do(t, "U_alice", http.MethodPost, "/api/groups/"+group+"/bills",
		fiber.Map{
			"title": "Lunch", "total": "200.00", "mode": "equal",
			"participants": []string{"U_alice", "U_bob"},
		})
	if status != http.StatusCreated {
		t.Fatalf("POST bill: %d %s", status, body)
	}

	spellings := []struct {
		name, id string
	}{
		{"canonical", group},
		{"uppercase", upper(group)},
		{"braced", braced(group)},
		{"unhyphenated", unhyphenated(group)},
	}
	for _, tc := range spellings {
		t.Run(tc.name, func(t *testing.T) {
			// A member reads the same group, and the same numbers, by any of its
			// names.
			status, body := ta.do(t, "U_alice", http.MethodGet, "/api/groups/"+tc.id+"/balances", nil)
			if status != http.StatusOK {
				t.Fatalf("GET balances at the %s spelling: %d %s, want 200", tc.name, status, body)
			}
			if net := ta.netOf(t, "U_alice", tc.id, "U_alice"); net != 10000 {
				t.Errorf("alice is at %s through the %s spelling, want 100.00", net, tc.name)
			}

			// The group the API reports is the canonical one, whichever spelling
			// was asked for. Anything else would put an attacker-chosen string
			// back into the client's hands to send again.
			status, body = ta.do(t, "U_alice", http.MethodGet, "/api/groups/"+tc.id, nil)
			if status != http.StatusOK {
				t.Fatalf("GET group at the %s spelling: %d %s, want 200", tc.name, status, body)
			}
			var g model.Group
			if err := json.Unmarshal(body, &g); err != nil {
				t.Fatal(err)
			}
			if g.ID != group {
				t.Errorf("the %s spelling reports id %q, want the canonical %q", tc.name, g.ID, group)
			}

			// And no spelling is a way in: requireMember runs on the canonical
			// value, so a non-member is refused by every one of them.
			status, body = ta.do(t, "U_stranger", http.MethodGet, "/api/groups/"+tc.id+"/balances", nil)
			if status != http.StatusNotFound {
				t.Errorf("a non-member reading balances at the %s spelling: %d %s, want 404",
					tc.name, status, body)
			}
		})
	}
}
