// Package handler implements the HTTP API.
package handler

import (
	"errors"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/OatApisit/billsplit-api/internal/config"
	"github.com/OatApisit/billsplit-api/internal/line"
	"github.com/OatApisit/billsplit-api/internal/middleware"
	"github.com/OatApisit/billsplit-api/internal/repo"
)

// Handler carries the dependencies every route needs.
type Handler struct {
	repo   *repo.Repo
	cfg    *config.Config
	pusher *line.Messenger
}

// New builds the API handler. pusher may be nil, in which case the endpoints
// that post back into a LINE chat report that they are not configured rather
// than failing at the call site.
func New(r *repo.Repo, cfg *config.Config, pusher *line.Messenger) *Handler {
	return &Handler{repo: r, cfg: cfg, pusher: pusher}
}

// Register mounts every route under the given router.
func (h *Handler) Register(api fiber.Router, auth *middleware.Auth) {
	api.Get("/health", h.health)

	protected := api.Group("", auth.Handler)
	protected.Get("/me", h.me)

	protected.Get("/groups", h.listGroups)
	protected.Post("/groups", h.createGroup)
	protected.Get("/groups/:id", h.getGroup)
	protected.Post("/groups/:id/members", h.joinGroup)

	protected.Get("/groups/:id/bills", h.listBills)
	protected.Post("/groups/:id/bills", h.createBill)
	protected.Delete("/groups/:id/bills/:billId", h.deleteBill)

	protected.Get("/groups/:id/balances", h.balances)
	protected.Get("/groups/:id/settlements", h.listSettlements)
	protected.Post("/groups/:id/settlements", h.createSettlement)
	protected.Delete("/groups/:id/settlements/:settlementId", h.deleteSettlement)
	protected.Post("/groups/:id/summary", h.pushSummary)
}

func (h *Handler) health(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{"status": "ok"})
}

func (h *Handler) me(c *fiber.Ctx) error {
	return c.JSON(middleware.CurrentUser(c))
}

// groupIDParam is the one place the :id path segment becomes a group ID.
//
// Fiber hands back the raw path segment, and a UUID has several spellings that
// Postgres reads as the same value: lowercase, uppercase, braced, unhyphenated.
// `WHERE id = $1` accepts all of them because the comparison happens after the
// uuid cast — which is harmless for a comparison and fatal for a *key*.
// repo.lockGroup hashes the group ID as text, so two requests naming one group
// in two spellings took two different advisory locks and serialised against
// nothing at all: two settlements of 100.00 against a single 100.00 debt, one
// path lowercase and one uppercase, were both admitted and turned the creditor
// into a debtor. Canonicalising is what makes one group one lock.
//
// It belongs here rather than inside lockGroup because the lock is only one
// consumer of this value. Fixing it there would leave the WHERE clauses, the
// membership check, and anything added later still reading a string the caller
// chose; past this line the group ID has exactly one spelling, and every
// consumer agrees on it by construction.
//
// A segment that is not a UUID at all cannot name a group, so it gets the same
// 404 a non-member gets — not a 400, which would tell a prober that their guess
// was at least the wrong shape.
func groupIDParam(c *fiber.Ctx) (string, error) {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return "", fiber.NewError(fiber.StatusNotFound, "group not found")
	}
	return id.String(), nil
}

// requireMember resolves the group in the URL, refusing callers who do not
// belong to it.
//
// Non-membership returns 404 rather than 403. A 403 confirms that the group
// exists, which lets anyone with a valid LINE account probe for real group IDs;
// to someone outside the group, a group they cannot see and a group that does
// not exist should be indistinguishable.
func (h *Handler) requireMember(c *fiber.Ctx) (string, string, error) {
	groupID, err := groupIDParam(c)
	if err != nil {
		return "", "", err
	}
	userID := middleware.CurrentUser(c).ID

	member, err := h.repo.IsMember(c.UserContext(), groupID, userID)
	if errors.Is(err, repo.ErrNotFound) {
		// Unreachable while groupIDParam runs first — it has already refused
		// anything Postgres would reject as a uuid. Kept because IsMember is
		// callable without it, and because the arm costs one line while a
		// malformed :id reaching Postgres bare is a 500 in front of every
		// group-scoped route.
		member, err = false, nil
	}
	if err != nil {
		return "", "", err
	}
	if !member {
		return "", "", fiber.NewError(fiber.StatusNotFound, "group not found")
	}
	return groupID, userID, nil
}

// notFoundAsHTTP converts a repo miss into a 404 and leaves other errors alone.
func notFoundAsHTTP(err error) error {
	if errors.Is(err, repo.ErrNotFound) {
		return fiber.NewError(fiber.StatusNotFound, "not found")
	}
	return err
}
