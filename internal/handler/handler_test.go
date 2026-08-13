package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OatApisit/billsplit-api/internal/config"
	"github.com/OatApisit/billsplit-api/internal/db"
	"github.com/OatApisit/billsplit-api/internal/line"
	"github.com/OatApisit/billsplit-api/internal/middleware"
	"github.com/OatApisit/billsplit-api/internal/model"
	"github.com/OatApisit/billsplit-api/internal/money"
	"github.com/OatApisit/billsplit-api/internal/repo"
)

// SHARED HARNESS — testApp and its helpers are used by every _test.go in this
// package (bill, balance, group). Extend them here rather than starting a
// second app fixture in the file you are working in.
//
// These tests drive the handlers through a real Fiber app and a real database.
//
// The unit tests beside them cover outstandingTo, which is only the arithmetic.
// What they cannot show is that createSettlement and deleteBill actually consult
// it: deleting either guard leaves a pure-logic suite entirely green, and a
// security control that can be removed without turning the build red will
// eventually be removed. Each integration test here is written so that removing
// the guard it covers fails it.
//
// Skipped unless TEST_DATABASE_URL points at a migrated database, which is
// truncated on entry — point it at a scratch database.

// userLocalsKey mirrors middleware's unexported userKey. authAs asserts against
// middleware.CurrentUser immediately after setting it, so a rename there fails
// these tests loudly instead of silently authenticating nobody.
const userLocalsKey = "billsplit.user"

// testApp is a Fiber app wired to the real handlers, with only the LINE token
// verification replaced. Everything under test — body parsing, requireMember,
// the guards, the status codes — is the production path.
type testApp struct {
	app  *fiber.App
	repo *repo.Repo
	ctx  context.Context

	// pool is the same pool repo is built on. The deadline and capacity tests
	// need it directly: one runs a query no repo function would ever write, and
	// another has to take every connection out of circulation to see what a
	// caller gets when there are none left.
	pool *pgxpool.Pool
}

// suiteLockKey names the exclusive lock every database-backed test holds for its
// duration. The identical helper lives in internal/repo's harness; the two
// packages share one database and `go test ./...` runs them at the same time, so
// without it that package's TRUNCATE lands in the middle of a test here —
// deadlocking against its open transactions, or simply deleting the group it is
// working on.
//
// It is taken in the two-argument form, which Postgres keeps in a different lock
// space from the one-argument pg_advisory_xact_lock(bigint) that repo.lockGroup
// uses. In one space they would be the same namespace, and a group whose
// hashtextextended happened to equal this key would block on the suite lock
// until the whole package finished. The odds are 1/2^64 and the cost of not
// having to think about them is one extra argument.
const (
	suiteLockClass = 0x5717
	suiteLockKey   = 0x1e5d
)

func holdSuiteLock(t *testing.T, pool *pgxpool.Pool, ctx context.Context) {
	t.Helper()

	// A session-level lock has to be taken and released on the same connection,
	// so it is held on one checked out of the pool for the test's lifetime.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire connection for the suite lock: %v", err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1, $2)`, suiteLockClass, suiteLockKey); err != nil {
		t.Fatalf("take the suite lock: %v", err)
	}
	t.Cleanup(func() {
		if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock($1, $2)`, suiteLockClass, suiteLockKey); err != nil {
			t.Errorf("release the suite lock: %v", err)
		}
		conn.Release()
	})
}

// callerHeader carries the authenticated caller for a test request.
//
// The stub reads it per request rather than from a field on testApp, because the
// race test fires two requests as two members at the same instant and a shared
// "who am I" field would both race and let one request authenticate as the
// other.
const callerHeader = "X-Test-Caller"

func newTestApp(t *testing.T) *testApp {
	t.Helper()

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping database tests")
	}

	ctx := context.Background()
	pool, err := db.Open(ctx, url)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(pool.Close)

	holdSuiteLock(t, pool, ctx)

	if _, err := pool.Exec(ctx, `
		TRUNCATE settlements, bill_shares, bills, group_members, groups, users CASCADE`); err != nil {
		t.Fatalf("truncate: %v (did you run the migration?)", err)
	}

	ta := &testApp{repo: repo.New(pool), ctx: ctx, pool: pool}
	h := New(ta.repo, &config.Config{}, stubPusher())

	ta.app = fiber.New()
	// The same middleware cmd/server mounts, with the same budget. Without it
	// c.UserContext() is context.Background() and these tests would exercise
	// handlers that no deadline applies to — which is the bug MY-8 fixed.
	ta.app.Use(middleware.RequestContext(middleware.DefaultRequestTimeout))
	api := ta.app.Group("/api", func(c *fiber.Ctx) error {
		caller := c.Get(callerHeader)
		c.Locals(userLocalsKey, model.User{ID: caller})
		if middleware.CurrentUser(c).ID != caller {
			t.Errorf("test auth stub is out of sync with middleware's user key")
			return fiber.NewError(fiber.StatusInternalServerError, "stub out of sync")
		}
		return c.Next()
	})
	h.registerForTest(api)

	return ta
}

// registerForTest mounts the routes under test directly. Handler.Register wraps
// them in middleware.Auth, which needs a live LINE channel to verify against;
// the stub above has already authenticated the caller, so everything from
// requireMember inwards is still the production path.
//
// Every route Register mounts that has a guard of its own belongs here.
// POST /groups is the one route requireMember cannot reach — its only defence is
// validateCreateGroup — so leaving it unmounted meant that guard could be
// deleted outright with the suite still green.
//
// This list is kept in step with Register by hand, which is the reason POST
// /groups/:id/summary and the two list routes were missing from it for three
// rounds; building the test app from Register itself is tracked as MY-16.
func (h *Handler) registerForTest(api fiber.Router) {
	api.Post("/groups", h.createGroup)
	api.Get("/groups/:id", h.getGroup)
	api.Post("/groups/:id/members", h.joinGroup)

	api.Get("/groups/:id/bills", h.listBills)
	api.Post("/groups/:id/bills", h.createBill)
	api.Delete("/groups/:id/bills/:billId", h.deleteBill)

	api.Get("/groups/:id/balances", h.balances)
	api.Get("/groups/:id/settlements", h.listSettlements)
	api.Post("/groups/:id/settlements", h.createSettlement)
	api.Delete("/groups/:id/settlements/:settlementId", h.deleteSettlement)
	api.Post("/groups/:id/summary", h.pushSummary)
}

// stubPusher is a Messenger whose HTTP client answers instead of LINE.
//
// A nil pusher would make pushSummary return 503 before either of its own
// guards ran, so mounting the route would prove nothing about them.
func stubPusher() *line.Messenger {
	m := line.NewMessenger("test-token")
	m.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader([]byte(`{}`))),
			Header:     http.Header{},
			Request:    r,
		}, nil
	})}
	return m
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// do issues a request as the given user and returns the status and body.
//
// It reports failures with t.Errorf and returns rather than t.Fatal, because the
// race tests call it from goroutines that are not the test's own: t.Fatal is
// documented as only valid on the goroutine running the test, and its
// runtime.Goexit there would kill the wrong goroutine, leak the WaitGroup it was
// counted into, and hang the suite instead of failing it. A zero status is not
// silent — every caller checks the status it expected.
func (ta *testApp) do(t *testing.T, as, method, path string, body any) (int, []byte) {
	t.Helper()

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Errorf("%s %s: marshal body: %v", method, path, err)
			return 0, nil
		}
		reader = bytes.NewReader(raw)
	}

	req := httptest.NewRequest(method, path, reader)
	req.Header.Set(callerHeader, as)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	res, err := ta.app.Test(req, -1)
	if err != nil {
		t.Errorf("%s %s: %v", method, path, err)
		return 0, nil
	}
	defer res.Body.Close()

	out, err := io.ReadAll(res.Body)
	if err != nil {
		t.Errorf("%s %s: read body: %v", method, path, err)
		return 0, nil
	}
	return res.StatusCode, out
}

func (ta *testApp) mustUser(t *testing.T, id string) model.User {
	t.Helper()
	u := model.User{ID: id, DisplayName: id}
	if err := ta.repo.UpsertUser(ta.ctx, u); err != nil {
		t.Fatalf("upsert %s: %v", id, err)
	}
	return u
}

// mustGroup creates a group with the given members and returns its ID.
func (ta *testApp) mustGroup(t *testing.T, name string, members ...string) string {
	t.Helper()
	for _, m := range members {
		ta.mustUser(t, m)
	}
	g, err := ta.repo.CreateGroup(ta.ctx, name, members[0], "")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range members[1:] {
		if err := ta.repo.AddMember(ta.ctx, g.ID, m); err != nil {
			t.Fatal(err)
		}
	}
	return g.ID
}

// netOf reads a member's net position back through the balances endpoint, which
// is the number the app actually shows.
func (ta *testApp) netOf(t *testing.T, as, groupID, userID string) money.Satang {
	t.Helper()
	status, body := ta.do(t, as, http.MethodGet, "/api/groups/"+groupID+"/balances", nil)
	if status != http.StatusOK {
		t.Fatalf("GET balances: %d %s", status, body)
	}

	var out struct {
		Balances []struct {
			User model.User   `json:"user"`
			Net  money.Satang `json:"net"`
		} `json:"balances"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	for _, b := range out.Balances {
		if b.User.ID == userID {
			return b.Net
		}
	}
	t.Fatalf("%s is not in the balances response: %s", userID, body)
	return 0
}

// A group ID that is not a UUID must be the same 404 a non-member gets.
// groupIDParam runs before every group-scoped handler, so this is the level the
// mapping has to hold at — repo.notFoundOnMalformedID on the delete queries is
// never reached.
//
// It must be a 404 and not a 400: a 400 says "the wrong shape", which is one bit
// more than a prober should get. GET /groups/:id is in the list because it does
// not go through requireMember — its authorisation is the query's WHERE clause —
// and until groupIDParam it answered a malformed ID with a bare 500.
func TestMalformedGroupIDIsNotFound(t *testing.T) {
	ta := newTestApp(t)
	ta.mustUser(t, "U_alice")

	paths := []struct {
		method, path string
	}{
		{http.MethodGet, "/api/groups/not-a-uuid"},
		{http.MethodPost, "/api/groups/not-a-uuid/members"},
		{http.MethodDelete, "/api/groups/not-a-uuid/bills/also-not-a-uuid"},
		{http.MethodGet, "/api/groups/not-a-uuid/bills"},
		{http.MethodGet, "/api/groups/not-a-uuid/balances"},
		{http.MethodGet, "/api/groups/not-a-uuid/settlements"},
		{http.MethodDelete, "/api/groups/not-a-uuid/settlements/x"},
		{http.MethodPost, "/api/groups/not-a-uuid/summary"},
	}
	for _, tc := range paths {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			status, body := ta.do(t, "U_alice", tc.method, tc.path, nil)
			if status != http.StatusNotFound {
				t.Errorf("got %d %s, want 404", status, body)
			}
		})
	}
}
