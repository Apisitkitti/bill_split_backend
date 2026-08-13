package handler

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OatApisit/billsplit-api/internal/middleware"
)

// MY-8. Nothing called fiber's SetUserContext, so every query in the project ran
// on context.Background(): no deadline, and no cancellation when the caller went
// away. These tests are the four things that were measured against this tree and
// have to stop being true.
//
// They use their own small Fiber apps rather than testApp's, because what is
// under test is the middleware and the pool rather than any one handler, and
// because two of them need a budget shorter than production's ten seconds to run
// in a reasonable time. newTestApp is still called first: it holds the suite
// lock and truncates, which every database-backed test in this package needs.

// A query that outlives its budget must end, and must say so in a status the
// caller can act on rather than a bare 500.
//
// Removing the SetUserContext call from middleware.RequestContext fails this:
// c.UserContext() falls back to context.Background(), the sleep runs to
// completion, and the request returns 200 five seconds later.
func TestAQueryPastItsDeadlineIsAGatewayTimeout(t *testing.T) {
	ta := newTestApp(t)

	const budget = 300 * time.Millisecond

	app := fiber.New()
	app.Use(middleware.RequestContext(budget))
	app.Get("/slow", func(c *fiber.Ctx) error {
		_, err := ta.pool.Exec(c.UserContext(), `SELECT pg_sleep(5)`)
		return err
	})

	start := time.Now()
	res, err := app.Test(httptest.NewRequest(http.MethodGet, "/slow", nil), -1)
	if err != nil {
		t.Fatalf("GET /slow: %v", err)
	}
	defer res.Body.Close()
	waited := time.Since(start)

	if res.StatusCode != http.StatusGatewayTimeout {
		t.Errorf("a query past its deadline returned %d, want %d",
			res.StatusCode, http.StatusGatewayTimeout)
	}
	if waited > time.Second {
		t.Errorf("the request took %v on a %v budget; the deadline is not reaching the query",
			waited, budget)
	}

	// The message is the one thing the client sees, and an unhandled database
	// error names the statement, the table, and sometimes the host.
	body := readBody(t, res)
	for _, leak := range []string{"pg_sleep", "SELECT", "sql", "context", "pgx", "5432"} {
		if strings.Contains(body, leak) {
			t.Errorf("the timeout message leaks %q: %s", leak, body)
		}
	}
}

// A client that hangs up must take its query with it.
//
// fasthttp will not tell us this by itself — RequestCtx.Done() closes only on
// server shutdown — so the request is served over a real socket here, and the
// client cancels mid-flight. What is asserted is the server side: the query has
// to come back with an error long before the thirty seconds it asked Postgres
// for. Deleting the watchDisconnect call fails this — the query runs on to the
// request deadline with nobody left to read it, holding a pooled connection the
// whole way.
func TestAClientThatHangsUpDoesNotLeaveItsQueryRunning(t *testing.T) {
	ta := newTestApp(t)

	type outcome struct {
		err   error
		spent time.Duration
	}
	done := make(chan outcome, 1)

	app := fiber.New()
	// Deliberately far longer than the test's patience: if the query stops, the
	// disconnect is the only thing that could have stopped it.
	app.Use(middleware.RequestContext(60 * time.Second))
	app.Get("/slow", func(c *fiber.Ctx) error {
		start := time.Now()
		_, err := ta.pool.Exec(c.UserContext(), `SELECT pg_sleep(30)`)
		done <- outcome{err: err, spent: time.Since(start)}
		return err
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = app.Listener(ln) }()
	t.Cleanup(func() { _ = app.Shutdown() })

	reqCtx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet,
		"http://"+ln.Addr().String()+"/slow", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Long enough that the query is certainly running, short enough that the
	// whole test is quick.
	time.AfterFunc(500*time.Millisecond, cancel)

	res, err := http.DefaultClient.Do(req)
	if err == nil {
		res.Body.Close()
		t.Fatal("the client's cancellation did not abort the request")
	}

	select {
	case got := <-done:
		if got.err == nil {
			t.Error("the query ran to completion even though the client had gone")
		}
		// The client left at 500ms and the connection is polled every 250ms, so
		// anything past a couple of seconds means the query was not cancelled
		// but simply finished, or was cancelled by something much later.
		if got.spent > 3*time.Second {
			t.Errorf("the query kept running %v after the client hung up", got.spent)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the query outlived its client by more than 10s; the disconnect cancelled nothing")
	}
}

// When every connection is checked out, the caller is told the server is busy —
// promptly, and not by being queued behind a wait nobody bounded.
//
// Removing the poolAcquireTimeout wrapper from repo fails this: pgxpool queues
// acquirers without limit, so the request waits on the pool until the request
// deadline runs out and reports a timeout it never actually spent doing
// anything.
func TestASaturatedPoolIsA503(t *testing.T) {
	ta := newTestApp(t)
	group := ta.mustGroup(t, "Dinner", "U_alice", "U_bob")

	held := takeEveryConnection(t, ta.pool)
	t.Logf("holding %d connections; the pool has nothing left to hand out", held)

	app := fiber.New()
	app.Use(middleware.RequestContext(middleware.DefaultRequestTimeout))
	app.Get("/probe", func(c *fiber.Ctx) error {
		_, err := ta.repo.HasBills(c.UserContext(), group)
		return err
	})

	start := time.Now()
	res, err := app.Test(httptest.NewRequest(http.MethodGet, "/probe", nil), -1)
	if err != nil {
		t.Fatalf("GET /probe: %v", err)
	}
	defer res.Body.Close()
	waited := time.Since(start)

	if res.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("a request against a saturated pool returned %d %s, want %d",
			res.StatusCode, readBody(t, res), http.StatusServiceUnavailable)
	}
	// Comfortably past repo's five-second acquire budget, and comfortably short
	// of the ten-second request budget that would swallow it if the acquire were
	// unbounded.
	if waited > 8*time.Second {
		t.Errorf("waited %v for a connection; the acquire is not bounded", waited)
	}
}

// THE ACCEPTANCE CRITERION.
//
// Measured against this tree before MY-8, with one group's advisory lock held by
// an outside session: ten in-flight settlements against that group took all ten
// pooled connections, and an unrelated group's membership check — one indexed
// row — was still blocked after fifteen seconds. One member, working only inside
// a group they created themselves, took the API down for everybody else.
//
// Three things had to be true for that, and each of the three is now bounded:
// the lock wait (groupLockTimeout), the connection wait (poolAcquireTimeout),
// and the request itself (middleware.DefaultRequestTimeout). The order between
// the first two is what this test is really about — the queue on the lock has to
// let go of its connections before an unrelated request gives up on getting one.
//
// Lengthening groupLockTimeout past poolAcquireTimeout fails this, and so does
// removing it: the unrelated group's read comes back 503 instead of 200.
func TestALockedGroupDoesNotBlockAnotherGroup(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping database tests")
	}

	ta := newTestApp(t)
	busy := ta.mustGroup(t, "Busy", "U_alice", "U_bob")
	other := ta.mustGroup(t, "Other", "U_alice", "U_bob")

	// The busy group is given a real debt, so its settlements would be admitted
	// if they ever got the lock. A burst that the bound would refuse anyway
	// would prove nothing about the lock.
	status, body := ta.do(t, "U_alice", http.MethodPost, "/api/groups/"+busy+"/bills",
		fiber.Map{
			"title": "Dinner", "total": "10000.00", "mode": "exact",
			"payerId":      "U_bob",
			"participants": []string{"U_alice"},
			"shares":       []string{"10000.00"},
		})
	if status != http.StatusCreated {
		t.Fatalf("POST bill on the busy group: %d %s", status, body)
	}

	// The lock is held from a session outside the pool, exactly as the report
	// measured it: a connection of its own, so that holding it does not itself
	// consume the capacity under test.
	outside, err := pgx.Connect(ta.ctx, url)
	if err != nil {
		t.Fatalf("connect an outside session: %v", err)
	}
	defer outside.Close(context.Background())
	if _, err := outside.Exec(ta.ctx,
		`SELECT pg_advisory_lock(hashtextextended($1::uuid::text, 0))`, busy); err != nil {
		t.Fatalf("hold the busy group's lock: %v", err)
	}

	// Ten, which is the pool's MaxConns: the whole pool, exactly as measured.
	const writers = 10

	var (
		wg     sync.WaitGroup
		codes  = make([]int, writers)
		bodies = make([][]byte, writers)
	)
	wg.Add(writers)
	for i := range writers {
		go func() {
			defer wg.Done()
			codes[i], bodies[i] = ta.do(t, "U_alice", http.MethodPost,
				"/api/groups/"+busy+"/settlements",
				fiber.Map{"toUser": "U_bob", "amount": "100.00"})
		}()
	}

	// Give the burst time to be in flight and queued on the lock before the
	// unrelated request goes in — the point is that it arrives at a pool that is
	// already gone, not that it beat the burst to it.
	time.Sleep(250 * time.Millisecond)

	unrelated := make(chan struct {
		code  int
		body  []byte
		spent time.Duration
	}, 1)
	go func() {
		start := time.Now()
		code, body := ta.do(t, "U_bob", http.MethodGet, "/api/groups/"+other+"/balances", nil)
		unrelated <- struct {
			code  int
			body  []byte
			spent time.Duration
		}{code, body, time.Since(start)}
	}()

	select {
	case got := <-unrelated:
		if got.code != http.StatusOK {
			t.Errorf("a member of an unrelated group got %d %s while another group was locked, want 200",
				got.code, got.body)
		}
		// The bound: it may wait for the lock queue to shed, which is
		// groupLockTimeout, and no longer.
		if got.spent > 6*time.Second {
			t.Errorf("an unrelated group's read waited %v behind a locked group", got.spent)
		} else {
			t.Logf("the unrelated group's read finished in %v", got.spent)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("an unrelated group's read was still blocked after 15s — this is the bug MY-8 exists to fix")
	}

	wg.Wait()

	// The losers were shed, and were told something they can act on. 503 and not
	// 500, so it does not read as a fault; and not 409, which on this path means
	// a settlement permanently blocks a bill and would send them round a retry
	// loop that can never succeed.
	for i, code := range codes {
		switch code {
		case http.StatusServiceUnavailable:
			if !strings.Contains(strings.ToLower(string(bodies[i])), "try again") {
				t.Errorf("writer %d: 503 without telling the caller to retry: %s", i, bodies[i])
			}
		case http.StatusCreated:
			t.Errorf("writer %d was admitted while the group's lock was held elsewhere", i)
		default:
			t.Errorf("writer %d: %d %s, want 503", i, code, bodies[i])
		}
	}
}

// takeEveryConnection checks out everything the pool can hand out and returns
// them at the end of the test.
func takeEveryConnection(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()

	stat := pool.Stat()
	free := int(stat.MaxConns() - stat.AcquiredConns())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conns := make([]*pgxpool.Conn, 0, free)
	t.Cleanup(func() {
		for _, c := range conns {
			c.Release()
		}
	})
	for i := 0; i < free; i++ {
		conn, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire %d of %d: %v", i+1, free, err)
		}
		conns = append(conns, conn)
	}
	return len(conns)
}

func readBody(t *testing.T, res *http.Response) string {
	t.Helper()
	buf := make([]byte, 512)
	n, _ := res.Body.Read(buf)
	return string(buf[:n])
}
