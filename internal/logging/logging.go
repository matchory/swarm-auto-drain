// Package logging configures the process-wide slog logger.
package logging

import (
	"log/slog"
	"os"
)

// Setup configures slog's default logger from the given level string
// ("debug" | "info" | "warn" | "error"). Unknown levels fall back to info.
func Setup(level string) {
	var l slog.Level
	switch level {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l})
	slog.SetDefault(slog.New(h))
}
