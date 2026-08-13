package handler

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5"
)

// MY-8, third round. THE PROPERTY: a response that instructs a retry must only
// follow a request that wrote nothing.
//
// Request deadlines created the way to break it. Before them c.UserContext() was
// context.Background(), so no transaction could be cut in the middle of its
// COMMIT; with them, a deadline landing in the commit round trip leaves Postgres
// committing while pgx returns the context error, asHTTP turns that into a 504,
// and the message asks the caller to try again. Measured at the production 10s
// budget: Bob pays Alice 100.00, gets "this took too long, please try again",
// taps again, and 200.00 is recorded for a 100.00 payment. The ledger still sums
// to zero, so nothing flags it, and only the sender may withdraw a settlement —
// so the member who benefits is the only one who can undo it.
//
// DELETE /bills wears the same bug as a different mask: the bill is deleted, the
// answer is 504, and the retry says 404, leaving the author sure their bill is
// still there.
//
// The reproduction is QA's: park the request's transaction on a lock held from an
// outside session, release it at a swept offset, and let the deadline walk
// through the transaction. What is asserted on every trial is the property, not
// the bug — so the test is silent when the property holds and loud the moment a
// committed write is answered with a retry.
//
// The lock is taken on the table each endpoint writes *last*, not on the group,
// and that is what makes the sweep cheap enough to run in a suite. Parked on the
// group lock, a transaction still has its ledger read and its insert to do after
// the release, and the commit is a sliver of the timeline: QA hit the window
// twice in 240 trials. Parked on its final statement, the only work left after
// the release is that statement finishing and the COMMIT, so the commit is a
// large fraction of what remains and the sweep finds it in tens of trials
// instead of hundreds.

const (
	// Short enough to run hundreds of trials, long enough that the request has
	// reached its blocked statement well before the deadline.
	sweepBudget = 100 * time.Millisecond

	// The release offset walks by this much per trial. Small enough that the
	// walk stays inside the commit round trip it is hunting for.
	sweepStep = 100 * time.Microsecond

	// Trials per endpoint, after calibration.
	sweepTrials = 60
)

// writeCase is one write endpoint, everything needed to put it at its commit
// boundary, and the question "did it write".
type writeCase struct {
	name string

	// blockTable is the table the endpoint's transaction touches last before
	// COMMIT, and blockMode the lock that stops it there.
	blockTable string
	blockMode  string

	// success is the status this endpoint returns when it did write.
	success int

	// setup prepares one trial and returns the request to make. It runs before
	// the block is taken.
	setup func(t *testing.T, ta *testApp, group string) (method, path string, body any)

	// wrote reports whether the trial's write is in the database.
	wrote func(t *testing.T, ta *testApp, group string) bool
}

// THE CRITICAL. A write cut at its commit boundary is either recorded and
// reported as recorded, or not recorded at all. There is no third answer, and in
// particular there is no "we could not do it, please try again" over a write that
// is already in the database.
//
// Reverting poolTx.Commit to commit on the caller's context fails this: the
// sweep finds trials that answer 504 with the row committed.
func TestAWriteCutAtItsCommitIsEitherRecordedOrReported(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping database tests")
	}

	ta := newTestApp(t)
	app := ta.appWithBudget(t, sweepBudget)

	// The block is held from a session outside the pool, so that holding it does
	// not itself consume the capacity under test.
	outside, err := pgx.Connect(ta.ctx, url)
	if err != nil {
		t.Fatalf("connect an outside session: %v", err)
	}
	defer outside.Close(context.Background())

	for _, tc := range writeCases() {
		t.Run(tc.name, func(t *testing.T) {
			trial := func(offset time.Duration) (code int, body []byte, wrote bool) {
				group := ta.mustGroup(t, "Trial", "U_alice", "U_bob")
				method, path, reqBody := tc.setup(t, ta, group)

				release := holdTable(t, ta.ctx, outside, tc.blockTable, tc.blockMode)

				done := make(chan struct{})
				go func() {
					defer close(done)
					code, body = ta.doOn(t, app, "U_bob", method, path, reqBody)
				}()

				time.Sleep(offset)
				release()
				<-done

				return code, body, tc.wrote(t, ta, group)
			}

			// Find where the outcome flips from "released in time" to "cut", so
			// the trials start at the boundary instead of walking to it.
			offset := findFlip(t, tc, trial)
			t.Logf("%s: the deadline reaches this transaction's commit around %v into a %v budget",
				tc.name, offset, sweepBudget)

			// From there the offset hunts the boundary rather than sweeping a
			// fixed band around it: back off a step when the write is cut, push
			// forward a step when it lands. A fixed band does not survive the
			// boundary moving — it moves by a millisecond or two as the database
			// warms and as the trials themselves load it, and a sweep centred
			// where the boundary *was* spends sixty trials on one side of it,
			// which is how POST /settlements came back "50 cut, 0 recorded" and
			// proved nothing. This tracks the drift instead, so every trial lands
			// within a step of the commit whatever the machine is doing.
			var cut, recorded, unknown int
			for n := 1; n <= sweepTrials; n++ {
				code, body, wrote := trial(offset)
				if wrote {
					recorded++
				}

				switch code {
				case tc.success:
					if !wrote {
						t.Fatalf("%s reported %d at offset %v, but nothing was written",
							tc.name, code, offset)
					}
					offset += sweepStep

				case http.StatusGatewayTimeout, http.StatusServiceUnavailable:
					cut++
					offset -= sweepStep
					// The premise: these are the answers that send the caller
					// round again. If one of them ever follows a committed
					// write, the retry records the money twice.
					if !strings.Contains(strings.ToLower(string(body)), "try again") {
						t.Fatalf("%s answered %d without telling the caller to retry: %s",
							tc.name, code, body)
					}
					if wrote {
						t.Fatalf("THE CRITICAL: %s answered %d %s on trial %d at offset %v, and the write is in the database. The caller retries and it is recorded twice.",
							tc.name, code, body, n, offset)
					}

				case http.StatusInternalServerError:
					// ErrOutcomeUnknown: allowed to have written or not, and
					// the only failure permitted to be ambiguous — so it is
					// the one that must not ask for a retry.
					unknown++
					offset -= sweepStep
					if strings.Contains(strings.ToLower(string(body)), "try again") {
						t.Fatalf("%s answered 500 with an unknown outcome and told the caller to try again: %s",
							tc.name, body)
					}

				default:
					t.Fatalf("%s: unexpected %d %s at offset %v", tc.name, code, body, offset)
				}

				if offset < 0 {
					offset = 0
				}
			}

			t.Logf("%s: %d trials, %d cut and reported as retryable, %d recorded, %d unknown-outcome",
				tc.name, sweepTrials, cut, recorded, unknown)

			// A sweep that never straddled the boundary proves nothing: every
			// trial was cut long before the commit, or every one finished long
			// before the deadline. Either way the calibration above missed and
			// the result is not evidence.
			if cut == 0 || recorded == 0 {
				t.Errorf("%s: the sweep never straddled the commit boundary (%d cut, %d recorded of %d) — it is not testing anything",
					tc.name, cut, recorded, sweepTrials)
			}
		})
	}
}

// writeCases is every endpoint that writes and can be told to try again.
//
// POST /groups is the fifth write in the API and is not here: it creates the
// group, so there is no group to set up around it and no member for whom a
// duplicate is money. Its commit goes through the same poolTx.Commit as these
// four.
func writeCases() []writeCase {
	return []writeCase{
		{
			// The likeliest victim in practice: the most common write, and the
			// retry duplicates a whole dinner across every member's balance.
			//
			// Blocked on bill_shares, which CreateBill writes after the bill
			// itself — with one participant that insert is the transaction's last
			// statement before COMMIT.
			name:       "POST bills",
			blockTable: "bill_shares",
			blockMode:  "SHARE",
			success:    http.StatusCreated,
			setup: func(t *testing.T, ta *testApp, group string) (string, string, any) {
				return http.MethodPost, "/api/groups/" + group + "/bills", fiber.Map{
					"title": "Dinner", "total": "100.00", "mode": "exact",
					"payerId":      "U_bob",
					"participants": []string{"U_alice"},
					"shares":       []string{"100.00"},
				}
			},
			wrote: func(t *testing.T, ta *testApp, group string) bool {
				return countIn(t, ta, `SELECT count(*) FROM bills WHERE group_id = $1`, group) > 0
			},
		},
		{
			// The reported case: Bob owes Alice 300.00, hands over 100 cash, taps
			// record once. Blocked on settlements, which CreateSettlement inserts
			// into after its ledger read.
			name:       "POST settlements",
			blockTable: "settlements",
			blockMode:  "SHARE",
			success:    http.StatusCreated,
			setup: func(t *testing.T, ta *testApp, group string) (string, string, any) {
				status, body := ta.do(t, "U_alice", http.MethodPost, "/api/groups/"+group+"/bills",
					fiber.Map{
						"title": "Dinner", "total": "300.00", "mode": "exact",
						"payerId":      "U_alice",
						"participants": []string{"U_bob"},
						"shares":       []string{"300.00"},
					})
				if status != http.StatusCreated {
					t.Fatalf("set up the debt: %d %s", status, body)
				}
				return http.MethodPost, "/api/groups/" + group + "/settlements",
					fiber.Map{"toUser": "U_alice", "amount": "100.00"}
			},
			wrote: func(t *testing.T, ta *testApp, group string) bool {
				return countIn(t, ta, `SELECT count(*) FROM settlements WHERE group_id = $1`, group) > 0
			},
		},
		{
			// The sub-case: cut this way, the bill is deleted and the answer is
			// 504, and the retry is a 404 that tells the author their bill is
			// still there. Blocked on settlements, which DeleteBill reads — the
			// dependency check — after the DELETE and before COMMIT. ACCESS
			// EXCLUSIVE because that read is a SELECT.
			name:       "DELETE bills",
			blockTable: "settlements",
			blockMode:  "ACCESS EXCLUSIVE",
			success:    http.StatusNoContent,
			setup: func(t *testing.T, ta *testApp, group string) (string, string, any) {
				return http.MethodDelete,
					"/api/groups/" + group + "/bills/" + mustBill(t, ta, group), nil
			},
			wrote: func(t *testing.T, ta *testApp, group string) bool {
				return countIn(t, ta, `SELECT count(*) FROM bills WHERE group_id = $1`, group) == 0
			},
		},
		{
			// One statement and a commit. Sent bare it was an implicit
			// transaction, which is why DeleteSettlement had to move into an
			// explicit one to be covered at all.
			name:       "DELETE settlements",
			blockTable: "settlements",
			blockMode:  "SHARE",
			success:    http.StatusNoContent,
			setup: func(t *testing.T, ta *testApp, group string) (string, string, any) {
				mustBill(t, ta, group)
				status, body := ta.do(t, "U_bob", http.MethodPost, "/api/groups/"+group+"/settlements",
					fiber.Map{"toUser": "U_alice", "amount": "50.00"})
				if status != http.StatusCreated {
					t.Fatalf("set up the settlement: %d %s", status, body)
				}
				var id string
				if err := ta.pool.QueryRow(ta.ctx,
					`SELECT id::text FROM settlements WHERE group_id = $1`, group).Scan(&id); err != nil {
					t.Fatalf("read back the settlement: %v", err)
				}
				return http.MethodDelete, "/api/groups/" + group + "/settlements/" + id, nil
			},
			wrote: func(t *testing.T, ta *testApp, group string) bool {
				return countIn(t, ta, `SELECT count(*) FROM settlements WHERE group_id = $1`, group) == 0
			},
		},
	}
}

// findFlip locates the release offset at which the request stops finishing in
// time and starts being cut, by bisection.
//
// It is measured rather than assumed because it is the sum of a membership
// check, a lock, a read and an insert on whatever machine this is running on.
// The commit boundary the sweep is looking for is immediately below it: the
// commit is the last thing the transaction does, so the last offsets that still
// succeed are the ones whose deadline lands inside it.
//
// The warm-up is not optional. Bisection cannot recover from a wrong answer, and
// the first request of a subtest pays for connections, plan caches and a cold
// route: DELETE /bills bisected to half the budget off one cold probe, and the
// sweep then spent fifty trials nowhere near the boundary and proved nothing.
func findFlip(t *testing.T, tc writeCase, trial func(time.Duration) (int, []byte, bool)) time.Duration {
	t.Helper()

	trial(0)                                // warm the fast path
	trial(sweepBudget + 5*time.Millisecond) // and the cut one

	lo, hi := time.Duration(0), sweepBudget
	for range 9 {
		mid := (lo + hi) / 2
		code, _, _ := trial(mid)
		if code == tc.success {
			lo = mid
		} else {
			hi = mid
		}
	}
	return hi
}

// holdTable takes a table lock in an outside transaction and returns the release.
//
// A table lock rather than the group's advisory lock because two of these four
// endpoints never take the group lock, and because it parks the transaction at
// the statement of our choosing — the last one before COMMIT — which is what
// makes the commit a findable fraction of the timeline.
func holdTable(t *testing.T, ctx context.Context, outside *pgx.Conn, table, mode string) func() {
	t.Helper()

	tx, err := outside.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the blocking transaction: %v", err)
	}
	// table and mode are constants in this file, never anything a caller sent.
	if _, err := tx.Exec(ctx, fmt.Sprintf("LOCK TABLE %s IN %s MODE", table, mode)); err != nil {
		t.Fatalf("lock %s: %v", table, err)
	}

	released := false
	t.Cleanup(func() {
		if !released {
			_ = tx.Rollback(context.Background())
		}
	})
	return func() {
		released = true
		if err := tx.Rollback(ctx); err != nil {
			t.Errorf("release the lock on %s: %v", table, err)
		}
	}
}

func countIn(t *testing.T, ta *testApp, query, group string) int {
	t.Helper()
	var n int
	if err := ta.pool.QueryRow(ta.ctx, query, group).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// mustBill records a bill Alice paid for Bob and returns its ID.
func mustBill(t *testing.T, ta *testApp, group string) string {
	t.Helper()

	status, body := ta.do(t, "U_bob", http.MethodPost, "/api/groups/"+group+"/bills",
		fiber.Map{
			"title": "Dinner", "total": "300.00", "mode": "exact",
			"payerId":      "U_alice",
			"participants": []string{"U_bob"},
			"shares":       []string{"300.00"},
		})
	if status != http.StatusCreated {
		t.Fatalf("set up the bill: %d %s", status, body)
	}

	var id string
	if err := ta.pool.QueryRow(ta.ctx,
		`SELECT id::text FROM bills WHERE group_id = $1`, group).Scan(&id); err != nil {
		t.Fatalf("read back the bill: %v", err)
	}
	return id
}
