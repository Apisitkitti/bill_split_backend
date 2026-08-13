package repo

import (
	"context"
	"errors"
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

// poolRow returns the connection once the single row has been scanned.
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

// poolTx returns the connection when the transaction ends, whichever way it
// ends.
type poolTx struct {
	pgx.Tx
	conn *pgxpool.Conn
}

func (t *poolTx) Commit(ctx context.Context) error {
	err := t.Tx.Commit(ctx)
	t.release()
	return err
}

func (t *poolTx) Rollback(ctx context.Context) error {
	err := t.Tx.Rollback(ctx)
	t.release()
	return err
}

func (t *poolTx) release() {
	if t.conn != nil {
		t.conn.Release()
		t.conn = nil
	}
}
