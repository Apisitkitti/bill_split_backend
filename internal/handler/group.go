package handler

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/gofiber/fiber/v2"

	"github.com/OatApisit/billsplit-api/internal/middleware"
	"github.com/OatApisit/billsplit-api/internal/repo"
)

type createGroupRequest struct {
	Name string `json:"name"`
	// LineGroupID comes from liff.getContext() when the app was opened inside
	// a LINE chat. Sending it binds the group to that chat so the bot can post
	// summaries there and so reopening finds the same group.
	LineGroupID string `json:"lineGroupId"`
}

// lineChatID matches the IDs LINE issues for a group or a multi-person room:
// "C" or "R" followed by 32 lowercase hex digits.
//
// This route is not group-scoped, so requireMember never runs on it and the
// value is unverified client input. Pinning the shape is what stops the field
// naming something that is not a chat at all — a user ID starts with "U", and
// LINE's push API accepts user, group, and room IDs interchangeably in "to",
// so without this check a caller could aim the official bot at any individual
// on the platform and have it deliver text of their choosing.
//
// The anchors are \A and \z rather than ^ and $ because they cannot be changed
// by a flag: ^ and $ become line anchors under (?m), and a pattern this one is
// later copied into or wrapped by would silently start accepting "C<32 hex>\n".
// Without (?m) Go's RE2 already treats $ as end of text — unlike Perl, PCRE and
// Python, where $ matches before a trailing newline — so this is about the
// pattern staying unambiguous, not about fixing a hole that ^...$ would leave.
var lineChatID = regexp.MustCompile(`\A[CR][0-9a-f]{32}\z`)

// maxGroupNameRunes caps the group name.
//
// The name is not only shown in the app: it is interpolated into the push
// notification's altText, which is what LINE renders on a lock screen. An
// unbounded name turns that line into as much attacker-controlled space as
// they care to fill. Real chat names sit far below this, so the cap is
// invisible to anyone naming a group honestly.
const maxGroupNameRunes = 100

// validateCreateGroup normalises and checks the request body in place.
//
// RESIDUAL RISK — chat membership is still unproven. A well-formed lineGroupId
// only shows that the value looks like a chat, not that the caller is in it.
// Someone who learns or guesses a chat ID can still register it before that
// chat's real members ever open the app; line_group_id is UNIQUE and no
// endpoint rebinds or deletes a group, so those members would then be silently
// enrolled into a group a stranger owns, permanently. Closing that needs the
// Messaging API to confirm membership, which needs a webhook this project does
// not have yet — see "Not built yet" in CLAUDE.md.
func validateCreateGroup(req *createGroupRequest) error {
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		return fiber.NewError(fiber.StatusBadRequest, "name is required")
	}
	if utf8.RuneCountInString(req.Name) > maxGroupNameRunes {
		return fiber.NewError(fiber.StatusBadRequest,
			fmt.Sprintf("name must be at most %d characters", maxGroupNameRunes))
	}

	req.LineGroupID = strings.TrimSpace(req.LineGroupID)
	if req.LineGroupID != "" && !lineChatID.MatchString(req.LineGroupID) {
		return fiber.NewError(fiber.StatusBadRequest,
			"lineGroupId is not a LINE group or room ID")
	}
	return nil
}

func (h *Handler) listGroups(c *fiber.Ctx) error {
	groups, err := h.repo.ListGroups(c.UserContext(), middleware.CurrentUser(c).ID)
	if err != nil {
		return err
	}
	return c.JSON(groups)
}

// createGroup makes a new group, or joins the existing one when the LINE chat
// already has a group.
//
// Reopening the LIFF app from a chat is the normal way back into a group, and
// it arrives here as another create request. Treating that as a conflict would
// mean every member had to be told "this chat already has a group, tap
// elsewhere"; instead the caller is enrolled and handed the group they meant.
func (h *Handler) createGroup(c *fiber.Ctx) error {
	var req createGroupRequest
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "malformed body")
	}

	if err := validateCreateGroup(&req); err != nil {
		return err
	}

	user := middleware.CurrentUser(c)

	if req.LineGroupID != "" {
		existing, err := h.repo.GroupByLineID(c.UserContext(), req.LineGroupID)
		switch {
		case err == nil:
			if err := h.repo.AddMember(c.UserContext(), existing.ID, user.ID); err != nil {
				return err
			}
			full, err := h.repo.GetGroup(c.UserContext(), existing.ID, user.ID)
			if err != nil {
				return notFoundAsHTTP(err)
			}
			return c.JSON(full)
		case !errors.Is(err, repo.ErrNotFound):
			return err
		}
	}

	group, err := h.repo.CreateGroup(c.UserContext(), req.Name, user.ID, req.LineGroupID)
	if err != nil {
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(group)
}

func (h *Handler) getGroup(c *fiber.Ctx) error {
	user := middleware.CurrentUser(c)
	group, err := h.repo.GetGroup(c.UserContext(), c.Params("id"), user.ID)
	if err != nil {
		return notFoundAsHTTP(err)
	}
	return c.JSON(group)
}

// joinGroup adds the caller to a group they have the ID for.
//
// The group ID is the invite: it is a UUID shared through the chat, and
// possessing it is what grants entry. That is deliberate for a group of
// friends splitting dinner, and it is the thing to revisit first if this ever
// holds something more sensitive than who owes whom for pizza.
//
// A group ID that does not exist gets the same 404 as one the caller may not
// see. Anything else would let a prober sort real IDs from invented ones by the
// status code alone.
func (h *Handler) joinGroup(c *fiber.Ctx) error {
	groupID := c.Params("id")
	user := middleware.CurrentUser(c)

	if err := h.repo.AddMember(c.UserContext(), groupID, user.ID); err != nil {
		return notFoundAsHTTP(err)
	}

	group, err := h.repo.GetGroup(c.UserContext(), groupID, user.ID)
	if err != nil {
		return notFoundAsHTTP(err)
	}
	return c.JSON(group)
}
