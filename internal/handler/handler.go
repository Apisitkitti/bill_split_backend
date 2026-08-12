// Package handler implements the HTTP API.
package handler

import (
	"errors"

	"github.com/gofiber/fiber/v2"

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

// requireMember resolves the group in the URL, refusing callers who do not
// belong to it.
//
// Non-membership returns 404 rather than 403. A 403 confirms that the group
// exists, which lets anyone with a valid LINE account probe for real group IDs;
// to someone outside the group, a group they cannot see and a group that does
// not exist should be indistinguishable.
func (h *Handler) requireMember(c *fiber.Ctx) (string, string, error) {
	groupID := c.Params("id")
	userID := middleware.CurrentUser(c).ID

	member, err := h.repo.IsMember(c.UserContext(), groupID, userID)
	if errors.Is(err, repo.ErrNotFound) {
		// A group ID that is not even a UUID. This runs before every
		// group-scoped handler, so without this arm a malformed :id would be a
		// 500 here and never reach the 404 mapping the delete routes rely on.
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
