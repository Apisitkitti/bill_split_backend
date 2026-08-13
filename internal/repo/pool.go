package repo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrPoolBusy reports that no pooled connection came free within
// poolAcquireTimeout. It is a capacity failure rather than a fault: the request
// was never sent to Postgres, so retrying it is safe and is what the caller
// should do. The handler layer turns it into a 503, which is why it must stay
// distinguishable from a query that ran and ran out of time.
var ErrPoolBusy = errors.New("repo: no database connection became free")

// poolAcquireTimeout bounds the wait for a connection out of the pool.
//
// db.Open caps the pool at 10 connections and puddle queues acquirers without
// limit, so before this existed a handful of slow writers turned every other
// request in the process into an unbounded wait: the queue was invisible, it
// grew for as long as the load lasted, and each waiter still held a request
// goroutine and a client socket. Fiber's 15s WriteTimeout ends the *response*,
// not the goroutine behind it, so nothing downstream ever shortened that wait.
//
// The value sits between the two other budgets on purpose. It is longer than
// groupLockTimeout, so a queue on one group's lock drains and gives its
// connections back before an unrelated request gives up on getting one — that
// ordering is what keeps one busy group from taking the API down for every other
// group. It is shorter than middleware.DefaultRequestTimeout, so a saturated
// pool is reported as a saturated pool rather than being swallowed by the
// request deadline and reported as a timeout.
//
// That last ordering — lock 2s < acquire 5s < request 10s — holds per operation,
// not per request. pushSummary makes six sequential acquires and createSettlement
// composes an acquire with a lock wait, so under saturation a later acquire in
// the same request has less than five seconds of request budget left. acquire
// then correctly declines to call it ErrPoolBusy (the caller's context is the one
// that ended) and the client gets the vaguer 504 instead of the 503 this budget
// was shaped to produce. Accepted: the alternative is a per-request budget
// divided among an unknown number of acquires, and 504 is still true.
const poolAcquireTimeout = 5 * time.Second

// boundedPool is pgxpool with the wait for a connection separated from the work
// done on it.
//
// pgxpool.Pool's own Query/Exec/Begin acquire a connection using the same
// context as the query, so bounding the acquire there would bound the query to
// the same deadline. A read that legitimately takes eight seconds and a pool
// that has nothing to hand out are different failures with different answers, so
// they get different budgets and different errors here.
//
// The Query/QueryRow/Begin wrappers mirror what pgxpool does internally: the
// connection is released when the rows are closed, the row is scanned, or the
// transaction is committed or rolled back. Release is idempotent, so the
// codebase's `defer rows.Close()` and `defer tx.Rollback(ctx)` keep working
// unchanged.
type boundedPool struct{ p *pgxpool.Pool }

// acquire checks out a connection, giving up rather than queueing forever.
func (d *boundedPool) acquire(ctx context.Context) (*pgxpool.Conn, error) {
	waitCtx, cancel := context.WithTimeout(ctx, poolAcquireTimeout)
	defer cancel()

	conn, err := d.p.Acquire(waitCtx)
	if err == nil {
		return conn, nil
	}
	// Only our own deadline means the pool is saturated. If the caller's context
	// is the one that ended, this is an ordinary request timeout or a client
	// that hung up, and reporting it as capacity would be a lie.
	if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		return nil, ErrPoolBusy
	}
	return nil, err
}

func (d *boundedPool) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	conn, err := d.acquire(ctx)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	defer conn.Release()
	return conn.Exec(ctx, sql, args...)
}

func (d *boundedPool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	conn, err := d.acquire(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := conn.Query(ctx, sql, args...)
	if err != nil {
		conn.Release()
		return nil, err
	}
	return &poolRows{Rows: rows, conn: conn}, nil
}

func (d *boundedPool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	conn, err := d.acquire(ctx)
	if err != nil {
		return errRow{err: err}
	}
	return &poolRow{row: conn.QueryRow(ctx, sql, args...), conn: conn}
}

func (d *boundedPool) Begin(ctx context.Context) (pgx.Tx, error) {
	conn, err := d.acquire(ctx)
	if err != nil {
		return nil, err
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		conn.Release()
		return nil, err
	}
	return &poolTx{Tx: tx, conn: conn}, nil
}

// poolRows returns the connection when the caller is finished reading.
type poolRows struct {
	pgx.Rows
	conn *pgxpool.Conn
}

func (r *poolRows) Close() {
	r.Rows.Close()
	r.release()
}

// Next and Scan release early on exhaustion or error, the way pgxpool's own
// wrapper does, so a caller that forgets Close still gives the connection back.
func (r *poolRows) Next() bool {
	more := r.Rows.Next()
	if !more {
		r.Close()
	}
	return more
}

func (r *poolRows) Scan(dest ...any) error {
	err := r.Rows.Scan(dest...)
	if err != nil {
		r.Close()
	}
	return err
}

func (r *poolRows) release() {
	if r.conn != nil {
		r.conn.Release()
		r.conn = nil
	}
}

// poolRow returns the connection once the single row has been scanned. Scan is
// the only release, so a QueryRow whose result is dropped without scanning
// strands a pooled connection for the life of the process — chain .Scan onto
// every QueryRow, as all four call sites and pgxpool's own wrapper do.
type poolRow struct {
	row  pgx.Row
	conn *pgxpool.Conn
}

func (r *poolRow) Scan(dest ...any) error {
	// Released even if Scan panics: a connection stranded by a panic is one the
	// pool never gets back, and ten of those are the whole pool.
	defer func() {
		if r.conn != nil {
			r.conn.Release()
			r.conn = nil
		}
	}()
	return r.row.Scan(dest...)
}

// errRow carries an acquire failure to the caller's Scan, which is the only
// place QueryRow can report one.
type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

// commitTimeout bounds a commit that has been detached from the request's
// deadline. It is the price of the detachment and has to be paid out of the gap
// between the request budget and the server's WriteTimeout.
//
// middleware.DefaultRequestTimeout is 10s and cmd/server's WriteTimeout is 15s,
// so a request cut at its budget may spend at most this long finishing its
// commit and still have its answer written: 10 + 3 = 13. Make it 5 and the worst
// case reaches the WriteTimeout, where the caller gets a dropped connection
// instead of a status — which is the failure this whole change exists to remove,
// wearing yet another mask.
//
// Three seconds is roughly a thousand times what a commit round trip costs on
// this schema, so it is not a budget honest work is expected to spend. It is the
// point past which the outcome stops being knowable, and past it we say exactly
// that rather than guessing; see ErrOutcomeUnknown.
const commitTimeout = 3 * time.Second

// rollbackTimeout bounds the rollback of a transaction whose own context has
// already expired. A ROLLBACK has no work to wait on — the transaction it undoes
// is idle by definition — so this covers one round trip and exists only so that
// a wedged connection cannot hold a request open past every other budget.
const rollbackTimeout = 3 * time.Second

// poolTx returns the connection when the transaction ends, whichever way it
// ends, and ends it on a context the request's deadline cannot cut.
type poolTx struct {
	pgx.Tx
	conn *pgxpool.Conn
}

// Commit finishes the transaction on a context detached from the caller's.
//
// This is the difference between "the write did not happen" and "the write may
// have happened and we told you to do it again". COMMIT is a round trip: cut the
// context while it is in flight and Postgres still commits, pgx still returns the
// context error, and the layer above turns that into a 504 whose message asks for
// a retry — so a settlement that was recorded gets recorded twice, and the ledger
// still sums to zero, so nothing flags it. Before request deadlines existed the
// window did not exist either, because no context could expire mid-commit.
//
// Detaching makes the outcome binary. Either the deadline fires before this line
// and the deferred Rollback undoes a transaction that wrote nothing, or the
// commit runs to completion and the caller is told what actually happened. There
// is no third state left in which a retry is both requested and unsafe — except
// the one commitTimeout marks explicitly.
//
// The cost is that a commit can outlive its request budget by up to
// commitTimeout, including across a shutdown: WithoutCancel drops the parent's
// cancellation, so ShutdownWithTimeout's 10s no longer stops a commit either.
// That is the intended trade — a commit interrupted by a shutdown has the same
// unknowable outcome as one interrupted by a deadline — and 3s fits inside that
// 10s with room to spare.
func (t *poolTx) Commit(ctx context.Context) error {
	commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commitTimeout)
	defer cancel()

	err := t.Tx.Commit(commitCtx)
	t.release()

	// Only our own bound running out leaves the outcome genuinely unknown. Every
	// other commit failure — a deferred constraint, a lost connection reported
	// before the commit was sent — is a transaction that did not commit.
	//
	// The underlying error is folded in with %v rather than %w on purpose: it
	// wraps context.DeadlineExceeded, and wrapping it here would make this
	// indistinguishable from an ordinary request timeout to the errors.Is that
	// picks the 504 — which is the exact message this arm exists to avoid.
	if err != nil && commitCtx.Err() != nil {
		return fmt.Errorf("%w: %v", ErrOutcomeUnknown, err)
	}
	return err
}

// Rollback undoes the transaction on a context detached from the caller's, for
// the same reason Commit commits on one: the deadline that ran out belongs to
// the work, not to the undoing of three statements' worth of nothing.
//
// pgx will not send a ROLLBACK on a context that is already done — it deadlines
// the socket, which closes the connection — and poolTx.release then hands the
// corpse to a pool that destroys it. The connection this rescues is the healthy
// one: a transaction refused by its own guard (DeleteBill's ErrSettlementDepends,
// CreateSettlement's bound, a lock_timeout) has nothing in flight, and before
// this it was still killed if the request's budget had run out in the meantime.
//
// It does NOT rescue a connection whose *statement* was cut, and the report that
// prompted this said otherwise. Measured on this tree: pgx's default context
// watcher deadlines the socket the moment the query's context expires, so the
// connection is already closed before Rollback is reached and no rollback context
// can bring it back. Making that case survivable means changing how the pool
// cancels — pgconn.CancelRequestContextWatcherHandler, which keeps the connection
// by dialling a second one to send the cancel and then holding the pooled
// connection ~110ms longer (measured). That trade is declined here: it spends an
// extra connection and an extra hundred milliseconds per shed request, in the one
// situation — a burst being shed — where connections and milliseconds are the
// scarce things. See TestARollbackPastTheDeadlineKeepsItsConnection.
func (t *poolTx) Rollback(ctx context.Context) error {
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()

	err := t.Tx.Rollback(rollbackCtx)
	t.release()
	return err
}

func (t *poolTx) release() {
	if t.conn != nil {
		t.conn.Release()
		t.conn = nil
	}
}
