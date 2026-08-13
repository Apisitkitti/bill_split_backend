// Package repo is the only place that talks SQL. Handlers deal in models; the
// queries live here so a schema change has one blast radius.
//
// The queries are split by the table they own — user.go, group.go, bill.go,
// settlement.go, ledger.go — and each has a _test.go of the same name. This
// file holds what all of them share: the Repo itself, the sentinel errors, and
// the Postgres error mapping.
package repo

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound reports a row that does not exist, or that the caller is not
// allowed to know exists.
var ErrNotFound = errors.New("repo: not found")

// ErrLedgerOverflow reports a group whose totals no longer survive the trip to
// the client as a JSON number. It is a data integrity failure, not a bad
// request, so it becomes a 500 rather than something the caller can fix.
var ErrLedgerOverflow = errors.New("repo: ledger total exceeds exact JSON range")

// ErrSettlementDepends reports a bill that cannot be withdrawn because the group
// holds a settlement recorded at or after it, which that bill may have been what
// justified.
//
// This is the other half of the reversibility argument behind DeleteBill. The
// author of a bill and the sender of a settlement can be the same person, and
// retracting only the bill turns a self-cancelling pair into a one-sided credit
// in their favour that the victim has no endpoint to undo. See deleteBill.
var ErrSettlementDepends = errors.New("repo: a recorded settlement depends on this bill")

// ErrGroupBusy reports that the group's writers are queued deeper than
// groupLockTimeout allows, so this one gave up without doing anything.
//
// It is a "come back in a moment", not a refusal: nothing about the request was
// wrong and nothing was written. It must stay distinct from ErrSettlementDepends
// — that one is a permanent no with a different remedy (the settlement's sender
// withdraws it first), and telling a caller to retry it would send them round a
// loop that can never succeed.
var ErrGroupBusy = errors.New("repo: the group is busy")

// foreignKeyViolation is the SQLSTATE Postgres returns when a row references a
// parent that is not there.
const foreignKeyViolation = "23503"

// lockNotAvailable is the SQLSTATE Postgres returns when lock_timeout expires
// with the lock still not granted.
const lockNotAvailable = "55P03"

// invalidTextRepresentation is the SQLSTATE Postgres returns for input that is
// not a valid value of the column's type — here, a path parameter that is not
// a UUID.
const invalidTextRepresentation = "22P02"

// notFoundOnMalformedID turns "invalid input syntax for type uuid" into a miss.
// A path parameter that is not a UUID cannot name a row, and a 500 there would
// tell a prober that their guess was at least the wrong shape.
func notFoundOnMalformedID(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == invalidTextRepresentation {
		return ErrNotFound
	}
	return err
}

// querier is the part of pgx that both the pool and a transaction implement, so
// a read can be written once and then run either on its own or inside the
// transaction whose decision depends on it.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// lockGroup serialises the group's ledger-changing writes against each other.
//
// Recording a settlement and withdrawing a bill each check a condition that the
// other one falsifies, and Postgres alone will not stop them: under READ
// COMMITTED the DELETE locks only the bills row and the INSERT locks only the
// settlements row, so the two transactions conflict on nothing, neither sees the
// other's uncommitted work, and both checks pass. The pair then commits into
// exactly the state each of them refused — the bill gone, the settlement that
// paid it still standing, and its recipient owing money for a payment nobody
// made. That outcome is unrecoverable: the recipient has no endpoint that undoes
// a settlement they did not send.
//
// An advisory lock keyed on the group is what makes the two orderings the only
// possible ones. It is taken as the first statement of both transactions, is
// held until commit or rollback, and blocks nothing outside the group — two
// different groups still settle in parallel.
//
// The key is derived from the group's *value*, not from the text it arrived as.
// hashtextextended hashes text, and a uuid has several spellings Postgres reads
// as one value, so hashing the caller's string handed one group two locks —
// 'A0EE…' and 'a0ee…' hash differently while `= $1` matches both, and two
// requests spelling the group differently serialised against nothing. The cast
// makes the key the canonical form of the same value every WHERE clause
// compares. Handlers canonicalise at the boundary too (see groupIDParam); this
// cast is what keeps the lock correct for a caller that does not.
// The wait for it is bounded, and that bound is the difference between a queue
// and an outage. A transaction parked on this lock is holding one of the pool's
// ten connections and making no progress, so ten members writing to one group
// while that group's lock is held by anything slow empties the pool — and a
// member of an entirely different group, whose own query touches one indexed
// row, then waits behind them for a lock they have nothing to do with. Measured
// on this tree with one group's lock held: ten in-flight settlements took all
// ten connections and an unrelated group's membership check was still blocked
// after fifteen seconds.
//
// groupLockTimeout ends that. A queue that is too deep sheds its tail instead of
// consuming the pool, and the loser is told to retry rather than being left
// hanging on a connection.
func lockGroup(ctx context.Context, tx pgx.Tx, groupID string) error {
	// SET LOCAL, so the bound expires with the transaction and cannot ride a
	// pooled connection into the next request that borrows it. set_config is
	// used rather than SET because SET cannot take a bind parameter, and the
	// alternative is formatting a number into SQL text.
	if _, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`,
		strconv.FormatInt(groupLockTimeout.Milliseconds(), 10)); err != nil {
		return err
	}

	// The cast rejects a group ID that is not a UUID, which is a miss rather
	// than a 500: the query this lock precedes could not have matched a row
	// either, and a prober must not learn that their guess was the wrong shape.
	_, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1::uuid::text, 0))`, groupID)

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == lockNotAvailable {
		// Nothing has been written — the lock is the transaction's first
		// statement — so this is safe to retry and says so.
		return ErrGroupBusy
	}
	return notFoundOnMalformedID(err)
}

// groupLockTimeout bounds how long a writer waits for its group's lock.
//
// It is deliberately the shortest of the three budgets. Waiting on this lock is
// the only one of them that is done *while holding a pooled connection*, so it
// has to expire well before poolAcquireTimeout: a request for an unrelated group
// gives up on the pool after five seconds, and the queue on this lock has to
// have released its connections by then or that request fails for a reason that
// has nothing to do with it.
//
// Two seconds is far more than honest contention needs. The group's writers hold
// the lock for one ledger read and one insert — single-digit milliseconds — so
// two seconds absorbs a queue hundreds deep before anyone is turned away, and
// the concurrency tests in internal/handler, which deliberately pile requests
// onto this lock, all still queue rather than shed. Shortening it until they
// start shedding would make those tests pass for the wrong reason.
const groupLockTimeout = 2 * time.Second

// Repo holds the queries for every table.
type Repo struct{ pool *boundedPool }

// New returns a Repo backed by the given pool.
func New(pool *pgxpool.Pool) *Repo { return &Repo{pool: &boundedPool{p: pool}} }
