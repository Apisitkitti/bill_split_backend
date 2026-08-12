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

	"github.com/OatApisit/billsplit-api/internal/config"
	"github.com/OatApisit/billsplit-api/internal/db"
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
	// as selects the authenticated caller for the next request.
	as string
}

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

	if _, err := pool.Exec(ctx, `
		TRUNCATE settlements, bill_shares, bills, group_members, groups, users CASCADE`); err != nil {
		t.Fatalf("truncate: %v (did you run the migration?)", err)
	}

	ta := &testApp{repo: repo.New(pool), ctx: ctx}
	h := New(ta.repo, &config.Config{}, nil)

	ta.app = fiber.New()
	api := ta.app.Group("/api", func(c *fiber.Ctx) error {
		user := model.User{ID: ta.as}
		c.Locals(userLocalsKey, user)
		if middleware.CurrentUser(c).ID != ta.as {
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
func (h *Handler) registerForTest(api fiber.Router) {
	api.Post("/groups/:id/bills", h.createBill)
	api.Delete("/groups/:id/bills/:billId", h.deleteBill)
	api.Get("/groups/:id/balances", h.balances)
	api.Post("/groups/:id/settlements", h.createSettlement)
	api.Delete("/groups/:id/settlements/:settlementId", h.deleteSettlement)
}

// do issues a request as the given user and returns the status and body.
func (ta *testApp) do(t *testing.T, as, method, path string, body any) (int, []byte) {
	t.Helper()
	ta.as = as

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}

	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	res, err := ta.app.Test(req, -1)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()

	out, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
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
// requireMember runs before every group-scoped handler, so this is the level the
// mapping has to hold at — repo.notFoundOnMalformedID on the delete queries is
// never reached.
func TestMalformedGroupIDIsNotFound(t *testing.T) {
	ta := newTestApp(t)
	ta.mustUser(t, "U_alice")

	paths := []struct {
		method, path string
	}{
		{http.MethodDelete, "/api/groups/not-a-uuid/bills/also-not-a-uuid"},
		{http.MethodGet, "/api/groups/not-a-uuid/balances"},
		{http.MethodDelete, "/api/groups/not-a-uuid/settlements/x"},
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
