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
// it is not reached at all. It must be mounted *below* fiber's logger — see
// mountMiddleware in cmd/server — or the translation below never runs.
//
// The parent is the fasthttp request context, so a server shutdown cancels
// in-flight queries too. A client that hangs up is *not* covered by that parent:
// fasthttp's RequestCtx.Done() closes only on shutdown, and nothing here tries to
// detect the disconnect itself. An earlier version of this file polled the socket
// with MSG_PEEK; it cancelled live requests — a client that half-closes after
// sending, which is legal and common, is indistinguishable from one that left —
// and it could not see through TLS at all. The timeout is the bound that holds:
// an abandoned request costs one connection for at most this long, without
// anyone guessing at socket state.
func RequestContext(timeout time.Duration) fiber.Handler {
	return func(c *fiber.Ctx) error {
		ctx, cancel := context.WithTimeout(c.Context(), timeout)
		defer cancel()

		c.SetUserContext(ctx)
		return asHTTP(c.Next())
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
func asHTTP(err error) error {
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
	//
	// Matched on err alone. Asking ctx.Err() as well relabelled *any* error that
	// happened to arrive after the deadline had fired — a validation 400 from a
	// slow handler included — as a timeout it was not.
	case errors.Is(err, context.DeadlineExceeded):
		return fiber.NewError(fiber.StatusGatewayTimeout,
			"this took too long, please try again")

	// Nothing cancels this context except a parent that was cancelled, and the
	// only thing that cancels the parent is ShutdownWithTimeout. So this is the
	// server going away mid-request, not the client — it used to answer 499
	// ("client closed request"), which was a lie about a caller who was still
	// waiting, and 499 has no other source now that nothing watches the socket.
	case errors.Is(err, context.Canceled):
		return fiber.NewError(fiber.StatusServiceUnavailable,
			"the server is restarting, please try again in a moment")
	}

	return err
}
