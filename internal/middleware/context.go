package middleware

import (
	"context"
	"errors"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/OatApisit/billsplit-api/internal/repo"
)

// DefaultRequestTimeout is how long any one request may spend in the database.
//
// It has to fit inside the 15s WriteTimeout in cmd/server, which is itself sized
// for LINE's in-app browser on mobile data: a response that is still being
// computed when WriteTimeout fires is never written at all, so the caller gets a
// dropped connection instead of a status they can act on. Ten seconds leaves
// five for the reply to reach a phone on a slow network — and a bill splitter
// whose queries touch one group has nothing that legitimately runs for ten
// seconds, so anything that does is already a fault rather than slow work.
const DefaultRequestTimeout = 10 * time.Second

// StatusClientClosedRequest is nginx's 499. There is no registered status for
// "the caller hung up", and by definition nobody reads this one; it exists so
// the access log distinguishes an abandoned request from a server fault.
const StatusClientClosedRequest = 499

// RequestContext gives every request a database context with a deadline, and
// translates the failures that deadline produces into statuses.
//
// Nothing in this codebase called fiber's SetUserContext, so c.UserContext()
// returned context.Background() and *no query anywhere had a deadline* — not the
// ledger reads, not the writes holding the group lock, despite comments saying
// the request context was used. A query with no deadline is one that keeps a
// pooled connection until Postgres finishes with it, however long that is and
// whether or not anyone is still waiting for the answer.
//
// It is mounted with app.Use rather than called from each handler, so it is not
// something a route added later can forget: a handler is reached through it or
// it is not reached at all.
//
// The parent is the fasthttp request context, so a server shutdown cancels
// in-flight queries too. Client disconnect is *not* covered by that parent —
// fasthttp's RequestCtx.Done() is closed only when the server is shutting down,
// which is what watchDisconnect exists to make up for.
func RequestContext(timeout time.Duration) fiber.Handler {
	return func(c *fiber.Ctx) error {
		ctx, cancel := context.WithTimeout(c.Context(), timeout)
		defer cancel()

		stop := watchDisconnect(c.Context().Conn(), cancel)
		defer stop()

		c.SetUserContext(ctx)
		return asHTTP(ctx, c.Next())
	}
}

// asHTTP gives the failures of a bounded request a status the caller can act on.
//
// Each of these was a bare 500 before, which tells the client nothing and tells
// a member on a phone to give up. None of them is a fault in the request:
// retrying is the right response to all three, and the statuses are kept
// distinct because the operator reading the logs needs to know which resource
// ran out.
//
// An error that already carries a status was chosen by a handler and is passed
// through untouched — in particular the 409 that refuses to withdraw a bill a
// settlement depends on, which is a permanent no and must never be confused with
// the 503 below that means "the group is busy, try again".
func asHTTP(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}

	var fiberErr *fiber.Error
	if errors.As(err, &fiberErr) {
		return err
	}

	switch {
	case errors.Is(err, repo.ErrPoolBusy):
		return fiber.NewError(fiber.StatusServiceUnavailable,
			"the server is busy right now, please try again in a moment")

	case errors.Is(err, repo.ErrGroupBusy):
		return fiber.NewError(fiber.StatusServiceUnavailable,
			"this group is busy right now, please try again in a moment")

	// The query ran and ran out of time. 504 rather than the 503s above so that
	// "we were too slow" stays separable from "we had no capacity to start", and
	// the message names neither the table nor the statement.
	case errors.Is(err, context.DeadlineExceeded), errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fiber.NewError(fiber.StatusGatewayTimeout,
			"this took too long, please try again")

	case errors.Is(err, context.Canceled), errors.Is(ctx.Err(), context.Canceled):
		return fiber.NewError(StatusClientClosedRequest, "client closed request")
	}

	return err
}
