// Package logging configures structured logging for the gateway.
package logging

import (
	"log/slog"
	"os"
	"strings"
)

// New returns a structured logger at the given level
// ("debug" | "info" | "warn" | "error"; unknown values fall back to info).
func New(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}
