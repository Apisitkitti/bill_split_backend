// Package repo is the only place that talks SQL. Handlers deal in models; the
// queries live here so a schema change has one blast radius.
//
// The queries are split by the table they own — user.go, group.go, bill.go,
// settlement.go, ledger.go — and each has a _test.go of the same name. This
// file holds what all of them share: the Repo itself, the sentinel errors, and
// the Postgres error mapping.
package repo

import (
	"errors"

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

// foreignKeyViolation is the SQLSTATE Postgres returns when a row references a
// parent that is not there.
const foreignKeyViolation = "23503"

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

// Repo holds the queries for every table.
type Repo struct{ pool *pgxpool.Pool }

// New returns a Repo backed by the given pool.
func New(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }
