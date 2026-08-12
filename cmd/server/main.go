// Command server runs the bill splitting API.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/joho/godotenv"

	"github.com/OatApisit/billsplit-api/internal/config"
	"github.com/OatApisit/billsplit-api/internal/db"
	"github.com/OatApisit/billsplit-api/internal/handler"
	"github.com/OatApisit/billsplit-api/internal/line"
	"github.com/OatApisit/billsplit-api/internal/middleware"
	"github.com/OatApisit/billsplit-api/internal/repo"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	if err := run(); err != nil {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	// A missing .env is fine: in production the environment is already set.
	_ = godotenv.Load(".env", ".env.local")

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	store := repo.New(pool)
	auth := middleware.NewAuth(line.NewVerifier(cfg.LineChannelID), store)

	var pusher *line.Messenger
	if cfg.PushEnabled() {
		pusher = line.NewMessenger(cfg.LineAccessToken)
	} else {
		slog.Warn("LINE_CHANNEL_ACCESS_TOKEN not set; chat summaries are disabled")
	}

	app := fiber.New(fiber.Config{
		AppName:      "billsplit",
		ErrorHandler: errorHandler,
		// LIFF runs behind LINE's in-app browser on mobile networks; the
		// defaults are tight enough to trip on a slow connection.
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
	})

	app.Use(recover.New())
	app.Use(logger.New(logger.Config{
		Format: "${time} ${status} ${latency} ${method} ${path}\n",
	}))
	app.Use(cors.New(cors.Config{
		AllowOrigins: strings.Join(cfg.AllowedOrigins, ","),
		AllowHeaders: "Origin, Content-Type, Accept, Authorization",
		AllowMethods: "GET, POST, PATCH, DELETE, OPTIONS",
	}))

	handler.New(store, cfg, pusher).Register(app.Group("/api"), auth)

	// Shut down on signal, giving in-flight requests a moment to finish.
	go func() {
		<-ctx.Done()
		slog.Info("shutting down")
		if err := app.ShutdownWithTimeout(10 * time.Second); err != nil {
			slog.Error("shutdown", "error", err)
		}
	}()

	slog.Info("listening", "port", cfg.Port)
	return app.Listen(":" + cfg.Port)
}

// errorHandler renders errors as JSON and keeps unexpected ones off the wire.
//
// A *fiber.Error carries a message this code chose to show the caller. Any
// other error is unplanned — a failed query, a nil dereference — and its text
// can name tables, columns, or hosts, so it is logged in full and reported as
// a bare 500.
func errorHandler(c *fiber.Ctx, err error) error {
	var fiberErr *fiber.Error
	if errors.As(err, &fiberErr) {
		return c.Status(fiberErr.Code).JSON(fiber.Map{"error": fiberErr.Message})
	}

	slog.Error("unhandled error",
		"error", err, "method", c.Method(), "path", c.Path())
	return c.Status(fiber.StatusInternalServerError).
		JSON(fiber.Map{"error": "internal server error"})
}
