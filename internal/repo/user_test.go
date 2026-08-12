package repo

import (
	"testing"

	"github.com/OatApisit/billsplit-api/internal/model"
)

// A LINE profile is not immutable: a member who renames themselves or changes
// their picture must show up under the new one on their next request, without
// the second sign-in failing on the unique line_user_id. That is what the
// ON CONFLICT arm is for, and nothing else in the suite exercises it.
func TestUpsertUserRefreshesTheProfile(t *testing.T) {
	r, ctx := newTestRepo(t)

	if err := r.UpsertUser(ctx, model.User{
		ID: "U_alice", DisplayName: "Alice", PictureURL: "https://example.test/old.jpg",
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.UpsertUser(ctx, model.User{
		ID: "U_alice", DisplayName: "Alice II", PictureURL: "https://example.test/new.jpg",
	}); err != nil {
		t.Fatalf("second upsert of the same user: %v", err)
	}

	// Members is the only read path back out to a display name.
	group, err := r.CreateGroup(ctx, "Dinner", "U_alice", "")
	if err != nil {
		t.Fatal(err)
	}
	members, err := r.Members(ctx, group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 {
		t.Fatalf("got %d members, want 1", len(members))
	}
	if members[0].DisplayName != "Alice II" {
		t.Errorf("display name is %q, want the refreshed %q", members[0].DisplayName, "Alice II")
	}
	if members[0].PictureURL != "https://example.test/new.jpg" {
		t.Errorf("picture is %q, want the refreshed one", members[0].PictureURL)
	}
}
