package repo

import (
	"context"
	"testing"
	"time"
)

// MY-8, third round. These two are the mechanism behind the property that
// internal/handler's commit-boundary sweep asserts end to end: a response that
// instructs a retry must only follow a request that wrote nothing.
//
// The sweep is the evidence a user would recognise — a real request, a real
// deadline walking through a real transaction — but it is a sweep, and it finds
// the window by looking for it. These are the same two claims stated directly,
// where they hold or fail on every run.

// A commit runs to completion even though the caller's deadline has already
// passed.
//
// This is the whole fix in one assertion. The deadline is allowed to arrive with
// the transaction's work done and its COMMIT not yet sent, which is the instant
// a real request spends in the commit round trip; from there the only two honest
// outcomes are "committed, and you were told so" and "nothing was written". A
// commit that inherits the expired context produces neither: Postgres commits,
// pgx reports the context error, and the caller is told to try again over a write
// that is already in the database.
//
// Reverting poolTx.Commit to `t.Tx.Commit(ctx)` fails this on every run: the
// commit returns the deadline error and the bill is not there.
func TestACommitIsNotCutByTheCallersDeadline(t *testing.T) {
	r, ctx := newTestRepo(t)

	alice := mustUser(t, r, ctx, "U_alice", "Alice")
	group, err := r.CreateGroup(ctx, "Dinner", alice.ID, "")
	if err != nil {
		t.Fatal(err)
	}

	deadline, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()

	tx, err := r.pool.Begin(deadline)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(deadline, `
		INSERT INTO bills (group_id, payer_id, title, total_satang, created_by)
		VALUES ($1, $2, 'Dinner', 10000, $2)`, group.ID, alice.ID); err != nil {
		t.Fatalf("insert inside the transaction: %v", err)
	}

	// The request budget runs out here, with the work finished and nothing
	// committed — the state a request is in for exactly one round trip.
	<-deadline.Done()

	if err := tx.Commit(deadline); err != nil {
		t.Fatalf("commit past the caller's deadline: %v — the caller is now told to retry a write whose outcome nobody knows", err)
	}

	var bills int
	if err := r.pool.QueryRow(ctx,
		`SELECT count(*) FROM bills WHERE group_id = $1`, group.ID).Scan(&bills); err != nil {
		t.Fatal(err)
	}
	if bills != 1 {
		t.Errorf("the commit reported success and stored %d bills, want 1", bills)
	}
}

// A rollback issued after the deadline has passed gives its connection back
// instead of destroying it.
//
// pgx reacts to a context that is already done by deadlining the socket, which
// closes the connection; poolTx.release then hands the corpse to a pool that
// destroys it, and the next request pays a fresh connect inside its own acquire
// budget. The connection here is *healthy* when the rollback is issued, which is
// the case worth saving and the common one on this codebase's write paths: a
// transaction refused by its own guard — DeleteBill's ErrSettlementDepends,
// CreateSettlement's bound, a lock_timeout — has nothing in flight, and the
// deferred `tx.Rollback(ctx)` was killing a connection over a deadline that had
// nothing to do with undoing three statements' worth of nothing.
//
// What this does NOT claim, because it is not true: that it saves a connection
// whose *statement* was cut. Measured on this tree — pgx has already closed that
// connection by the time Rollback is reached, so no rollback context can rescue
// it. See the note on poolTx.Rollback.
//
// Reverting poolTx.Rollback to `t.Tx.Rollback(ctx)` fails this: every cycle
// constructs a replacement connection.
func TestARollbackPastTheDeadlineKeepsItsConnection(t *testing.T) {
	r, ctx := newTestRepo(t)

	// Warm one connection, so the pool has something to reuse and a
	// construction afterwards can only mean the reused one was destroyed.
	var warm int
	if err := r.pool.QueryRow(ctx, `SELECT 1`).Scan(&warm); err != nil {
		t.Fatal(err)
	}
	before := r.pool.p.Stat().NewConnsCount()

	const cycles = 4
	for i := range cycles {
		func() {
			cut, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
			defer cancel()

			tx, err := r.pool.Begin(cut)
			if err != nil {
				t.Fatalf("begin %d: %v", i, err)
			}

			var one int
			if err := tx.QueryRow(cut, `SELECT 1`).Scan(&one); err != nil {
				t.Fatalf("query %d: %v", i, err)
			}

			// The budget runs out with the transaction open, its work done and
			// its connection idle — a guard about to refuse, on a request that
			// has taken too long.
			<-cut.Done()
			if err := tx.Rollback(cut); err != nil {
				t.Fatalf("rollback %d past the deadline: %v", i, err)
			}
		}()

		// Reuse the pool, which is where a destroyed connection shows up.
		var n int
		if err := r.pool.QueryRow(ctx, `SELECT 1`).Scan(&n); err != nil {
			t.Fatalf("query the pool after cycle %d: %v", i, err)
		}
	}

	if built := r.pool.p.Stat().NewConnsCount() - before; built > 0 {
		t.Errorf("%d deadline-late rollbacks cost %d fresh connections; each one is a connect inside some later request's acquire budget",
			cycles, built)
	}
}
