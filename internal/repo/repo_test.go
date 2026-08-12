package repo

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/OatApisit/billsplit-api/internal/db"
	"github.com/OatApisit/billsplit-api/internal/model"
)

// SHARED HARNESS — newTestRepo and mustUser are used by every _test.go in this
// package (user, group, bill, settlement, ledger). Add to them here rather than
// copying them into the file you are working in.
//
// These tests exercise real SQL, which is the half of the balance logic that
// unit tests over money and settle cannot reach: the UNION in Ledger is where
// a wrong join or a dropped arm would silently misreport who owes what.
//
// They are skipped unless TEST_DATABASE_URL points at a migrated database:
//
//	make db && make migrate && make itest
//
// The database is truncated on entry, so point this at a scratch database.
func newTestRepo(t *testing.T) (*Repo, context.Context) {
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

	_, err = pool.Exec(ctx, `
		TRUNCATE settlements, bill_shares, bills, group_members, groups, users CASCADE`)
	if err != nil {
		t.Fatalf("truncate: %v (did you run the migration?)", err)
	}

	return New(pool), ctx
}

func mustUser(t *testing.T, r *Repo, ctx context.Context, id, name string) model.User {
	t.Helper()
	u := model.User{ID: id, DisplayName: name}
	if err := r.UpsertUser(ctx, u); err != nil {
		t.Fatalf("upsert %s: %v", id, err)
	}
	return u
}

// A path parameter that is not a UUID must be a miss, not a 500. Postgres
// rejects it with a syntax error, and letting that through would both leak the
// backend and tell a prober their guess was the wrong shape.
func TestDeleteWithMalformedIDIsNotFound(t *testing.T) {
	r, ctx := newTestRepo(t)

	alice := mustUser(t, r, ctx, "U_alice", "Alice")
	group, err := r.CreateGroup(ctx, "Dinner", alice.ID, "")
	if err != nil {
		t.Fatal(err)
	}

	if err := r.DeleteBill(ctx, group.ID, "not-a-uuid", alice.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteBill with a malformed ID: got %v, want ErrNotFound", err)
	}
	if err := r.DeleteSettlement(ctx, group.ID, "not-a-uuid", alice.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteSettlement with a malformed ID: got %v, want ErrNotFound", err)
	}
}
