package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/OatApisit/billsplit-api/internal/db"
)

// MY-8. The middleware *order* is part of the contract, so it is tested through
// the same function the server calls rather than through a chain assembled here.
//
// The bug this exists to catch shipped once already: RequestContext was mounted
// above fiber's logger, and logger renders a handler error through
// app.ErrorHandler itself and then returns nil, so RequestContext saw nil from
// c.Next() and translated nothing. Every deadline, ErrPoolBusy and ErrGroupBusy
// reached the client as a bare 500. Nothing caught it because every test in
// internal/handler builds its own bare fiber.New() with no logger — the
// middleware was correct in isolation and dead in the binary.
//
// Putting app.Use(middleware.RequestContext(...)) back above logger in
// mountMiddleware fails this with 500 instead of 504.
func TestADeadlineIsA504ThroughTheServersOwnMiddlewareChain(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping database tests")
	}

	ctx := context.Background()
	pool, err := db.Open(ctx, url)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	defer pool.Close()

	// The server's own ErrorHandler, because what is under test is the whole path
	// an error takes from the handler to the wire.
	app := fiber.New(fiber.Config{ErrorHandler: errorHandler})

	const budget = 300 * time.Millisecond
	mountMiddleware(app, []string{"http://localhost:5173"}, budget)

	// Below the chain, exactly where Register mounts the real routes.
	app.Get("/slow", func(c *fiber.Ctx) error {
		_, err := pool.Exec(c.UserContext(), `SELECT pg_sleep(5)`)
		return err
	})

	start := time.Now()
	res, err := app.Test(httptest.NewRequest(http.MethodGet, "/slow", nil), -1)
	if err != nil {
		t.Fatalf("GET /slow: %v", err)
	}
	defer res.Body.Close()
	waited := time.Since(start)

	buf := make([]byte, 512)
	n, _ := res.Body.Read(buf)
	body := string(buf[:n])

	if res.StatusCode != http.StatusGatewayTimeout {
		t.Errorf("a query past its deadline returned %d %s through the server's chain, want %d",
			res.StatusCode, body, http.StatusGatewayTimeout)
	}
	if strings.Contains(body, "internal server error") {
		t.Errorf("the error was rendered before RequestContext could translate it: %s", body)
	}
	if waited > time.Second {
		t.Errorf("the request took %v on a %v budget; the deadline is not reaching the query",
			waited, budget)
	}
}
