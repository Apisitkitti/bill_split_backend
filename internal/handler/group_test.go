package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/OatApisit/billsplit-api/internal/model"
)

// The unit tests below prove validateCreateGroup rejects a user ID. This one
// proves createGroup actually calls it.
//
// POST /groups is the only route in the API that requireMember cannot guard —
// there is no group yet — so validateCreateGroup is its entire defence, and a
// lineGroupId of "U..." aims the official LINE bot at an individual. Deleting
// the validateCreateGroup call from createGroup fails this test.
func TestCreateGroupRejectsAUserIDAsTheChatID(t *testing.T) {
	ta := newTestApp(t)
	ta.mustUser(t, "U_alice")

	status, body := ta.do(t, "U_alice", http.MethodPost, "/api/groups",
		fiber.Map{"name": "Dinner", "lineGroupId": "U1234567890abcdef1234567890abcdef"})
	if status != http.StatusBadRequest {
		t.Fatalf("POST /groups with a user ID as lineGroupId: %d %s, want 400", status, body)
	}

	groups, err := ta.repo.ListGroups(ta.ctx, "U_alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 0 {
		t.Errorf("got %d groups after the refused create, want 0", len(groups))
	}
}

// A blank name is refused on the same route, for the same reason: the name
// reaches a lock screen and nothing downstream re-checks it.
func TestCreateGroupRejectsABlankName(t *testing.T) {
	ta := newTestApp(t)
	ta.mustUser(t, "U_alice")

	status, body := ta.do(t, "U_alice", http.MethodPost, "/api/groups",
		fiber.Map{"name": "   ", "lineGroupId": ""})
	if status != http.StatusBadRequest {
		t.Fatalf("POST /groups with a blank name: %d %s, want 400", status, body)
	}

	groups, err := ta.repo.ListGroups(ta.ctx, "U_alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 0 {
		t.Errorf("got %d groups after the refused create, want 0", len(groups))
	}
}

// The guard must not have swallowed the honest path: a real chat ID creates the
// group, and a second member joins it through POST /groups/:id/members.
func TestCreateGroupAcceptsAChatIDAndAdmitsAJoiner(t *testing.T) {
	ta := newTestApp(t)
	ta.mustUser(t, "U_alice")
	ta.mustUser(t, "U_bob")

	status, body := ta.do(t, "U_alice", http.MethodPost, "/api/groups",
		fiber.Map{"name": "  Dinner  ", "lineGroupId": "C1234567890abcdef1234567890abcdef"})
	if status != http.StatusCreated {
		t.Fatalf("POST /groups: %d %s, want 201", status, body)
	}
	var group model.Group
	if err := json.Unmarshal(body, &group); err != nil {
		t.Fatal(err)
	}
	if group.Name != "Dinner" {
		t.Errorf("stored name is %q, want %q — validateCreateGroup trims in place", group.Name, "Dinner")
	}

	status, body = ta.do(t, "U_bob", http.MethodPost, "/api/groups/"+group.ID+"/members", nil)
	if status != http.StatusOK {
		t.Fatalf("POST members: %d %s, want 200", status, body)
	}
	if net := ta.netOf(t, "U_bob", group.ID, "U_bob"); net != 0 {
		t.Errorf("a joiner with no bills is at %s, want 0.00", net)
	}
}

// lineGroupId arrives on a route that requireMember never guards, and it ends
// up as the "to" of a bot push. LINE accepts user, group, and room IDs
// interchangeably there, so anything that is not a group or room ID must not
// get through: a "U..." value would let the official bot deliver
// attacker-written text to an individual.
func TestValidateCreateGroupLineChatID(t *testing.T) {
	const (
		groupID = "C1234567890abcdef1234567890abcdef"
		roomID  = "Rfedcba0987654321fedcba0987654321"
		userID  = "U1234567890abcdef1234567890abcdef"
	)

	accepted := []struct {
		name, id string
	}{
		{"group id", groupID},
		{"room id", roomID},
		{"absent, browser opened", ""},
		{"surrounding whitespace is trimmed", "  " + groupID + "  "},
	}
	for _, tc := range accepted {
		t.Run("accept/"+tc.name, func(t *testing.T) {
			req := createGroupRequest{Name: "Dinner", LineGroupID: tc.id}
			if err := validateCreateGroup(&req); err != nil {
				t.Fatalf("rejected %q: %v", tc.id, err)
			}
			if req.LineGroupID != strings.TrimSpace(tc.id) {
				t.Errorf("normalised to %q, want %q", req.LineGroupID, strings.TrimSpace(tc.id))
			}
		})
	}

	rejected := []struct {
		name, id string
	}{
		{"user id", userID},
		{"user id lowercase prefix", "u1234567890abcdef1234567890abcdef"},
		{"uppercase hex", "C1234567890ABCDEF1234567890ABCDEF"},
		{"one digit short", groupID[:len(groupID)-1]},
		{"one digit long", groupID + "0"},
		{"non-hex body", "Czzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"},
		{"prefix only", "C"},
		{"arbitrary text", "not-a-chat"},
		{"interior newline", "C1234567890abcdef\n1234567890abcdef"},
		{"a second ID appended after a newline", groupID + "\n" + userID},
	}
	for _, tc := range rejected {
		t.Run("reject/"+tc.name, func(t *testing.T) {
			req := createGroupRequest{Name: "Dinner", LineGroupID: tc.id}
			err := validateCreateGroup(&req)
			if err == nil {
				t.Fatalf("accepted %q as a LINE chat ID", tc.id)
			}
			assertStatus(t, err, fiber.StatusBadRequest)
		})
	}
}

// A trailing newline must not get through to LINE's "to" field on its own,
// independently of the trim in validateCreateGroup.
//
// This does not demonstrate anything about \A/\z over ^/$: in Go's RE2, without
// (?m), `^[CR][0-9a-f]{32}$` rejects this input identically. The anchors are
// spelled \A and \z so that no later (?m) can turn them into line anchors, and
// this test only pins the behaviour, not the reason for the spelling.
func TestLineChatIDRejectsTrailingNewline(t *testing.T) {
	if lineChatID.MatchString("C1234567890abcdef1234567890abcdef\n") {
		t.Error("pattern matched a value with a trailing newline")
	}
}

// The name reaches a lock screen through the push altText, so its length is
// bounded. The boundary itself is counted in runes: Thai group names are the
// normal case and a byte cap would cut them at a third of the length.
func TestValidateCreateGroupName(t *testing.T) {
	t.Run("empty after trim", func(t *testing.T) {
		req := createGroupRequest{Name: "   \t "}
		err := validateCreateGroup(&req)
		if err == nil {
			t.Fatal("accepted a blank name")
		}
		assertStatus(t, err, fiber.StatusBadRequest)
	})

	t.Run("at the cap", func(t *testing.T) {
		req := createGroupRequest{Name: strings.Repeat("ก", maxGroupNameRunes)}
		if err := validateCreateGroup(&req); err != nil {
			t.Fatalf("rejected a name of exactly %d runes: %v", maxGroupNameRunes, err)
		}
	})

	t.Run("one past the cap", func(t *testing.T) {
		req := createGroupRequest{Name: strings.Repeat("ก", maxGroupNameRunes+1)}
		err := validateCreateGroup(&req)
		if err == nil {
			t.Fatalf("accepted a name of %d runes", maxGroupNameRunes+1)
		}
		assertStatus(t, err, fiber.StatusBadRequest)
	})

	t.Run("trimmed before storing", func(t *testing.T) {
		req := createGroupRequest{Name: "  Dinner  "}
		if err := validateCreateGroup(&req); err != nil {
			t.Fatal(err)
		}
		if req.Name != "Dinner" {
			t.Errorf("name is %q, want %q", req.Name, "Dinner")
		}
	})
}

func assertStatus(t *testing.T, err error, want int) {
	t.Helper()
	fe, ok := err.(*fiber.Error)
	if !ok {
		t.Fatalf("got %T (%v), want *fiber.Error — anything else becomes a 500", err, err)
	}
	if fe.Code != want {
		t.Errorf("status %d, want %d", fe.Code, want)
	}
}
