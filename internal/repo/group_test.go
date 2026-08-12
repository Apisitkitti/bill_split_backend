package repo

import (
	"errors"
	"testing"
)

// Membership is enforced in the query itself, so a non-member must not be able
// to read a group even with its ID.
func TestGetGroupHidesFromNonMembers(t *testing.T) {
	r, ctx := newTestRepo(t)

	alice := mustUser(t, r, ctx, "U_alice", "Alice")
	stranger := mustUser(t, r, ctx, "U_stranger", "Stranger")

	group, err := r.CreateGroup(ctx, "Private", alice.ID, "")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := r.GetGroup(ctx, group.ID, alice.ID); err != nil {
		t.Errorf("creator cannot read own group: %v", err)
	}
	if _, err := r.GetGroup(ctx, group.ID, stranger.ID); err == nil {
		t.Error("stranger read a group they do not belong to")
	}
}

// Reopening the LIFF app from the same chat must find the same group.
func TestGroupByLineID(t *testing.T) {
	r, ctx := newTestRepo(t)

	alice := mustUser(t, r, ctx, "U_alice", "Alice")
	created, err := r.CreateGroup(ctx, "Chat group", alice.ID, "C_linegroup")
	if err != nil {
		t.Fatal(err)
	}

	found, err := r.GroupByLineID(ctx, "C_linegroup")
	if err != nil {
		t.Fatal(err)
	}
	if found.ID != created.ID {
		t.Errorf("got group %s, want %s", found.ID, created.ID)
	}

	if _, err := r.GroupByLineID(ctx, "C_nonexistent"); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

// Joining a group ID that does not exist must look exactly like joining one the
// caller may not see. A raw foreign key violation here surfaces as a 500, and
// the difference from the 404 a non-member gets is enough to tell a prober
// which UUIDs are real.
func TestAddMemberToMissingGroup(t *testing.T) {
	r, ctx := newTestRepo(t)

	alice := mustUser(t, r, ctx, "U_alice", "Alice")

	const ghostGroup = "00000000-0000-0000-0000-000000000000"
	if err := r.AddMember(ctx, ghostGroup, alice.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("AddMember to a missing group: got %v, want ErrNotFound", err)
	}
}
