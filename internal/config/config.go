// Package config loads runtime settings from the environment and refuses to
// start without the ones that matter.
package config

import (
	"fmt"
	"os"
	"strings"
)

// Config holds everything the server needs to run.
type Config struct {
	Port        string
	DatabaseURL string

	// LineChannelID is the LINE Login channel the LIFF app belongs to. ID
	// tokens are pinned to it.
	LineChannelID string
	// LineChannelSecret signs incoming webhooks from the Messaging API channel.
	LineChannelSecret string
	// LineAccessToken authenticates outgoing pushes on the Messaging API channel.
	LineAccessToken string

	// LiffURL is the permanent link to the LIFF app, used in Flex buttons.
	LiffURL string
	// AllowedOrigins lists the origins permitted to call this API.
	AllowedOrigins []string
}

// Load reads configuration from the environment.
//
// Missing required values are collected and reported together: discovering
// them one restart at a time is the slowest possible way to configure a
// service.
func Load() (*Config, error) {
	cfg := &Config{
		Port:              env("PORT", "8080"),
		DatabaseURL:       os.Getenv("DATABASE_URL"),
		LineChannelID:     os.Getenv("LINE_CHANNEL_ID"),
		LineChannelSecret: os.Getenv("LINE_CHANNEL_SECRET"),
		LineAccessToken:   os.Getenv("LINE_CHANNEL_ACCESS_TOKEN"),
		LiffURL:           os.Getenv("LIFF_URL"),
		AllowedOrigins:    splitList(env("ALLOWED_ORIGINS", "http://localhost:5173")),
	}

	var missing []string
	if cfg.DatabaseURL == "" {
		missing = append(missing, "DATABASE_URL")
	}
	if cfg.LineChannelID == "" {
		missing = append(missing, "LINE_CHANNEL_ID")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("config: missing required environment variables: %s",
			strings.Join(missing, ", "))
	}
	return cfg, nil
}

// PushEnabled reports whether outgoing Messaging API calls are configured.
// Bills and settlements work without it; only the "post the summary back to
// the chat" feature needs a Messaging API channel.
func (c *Config) PushEnabled() bool { return c.LineAccessToken != "" }

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func splitList(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
