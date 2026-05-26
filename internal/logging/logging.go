// Package logging configures the process-wide slog logger.
package logging

import (
	"log/slog"
	"os"
)

// Setup configures slog's default logger from the given level string
// ("debug" | "info" | "warn" | "error"). Unknown levels fall back to info.
func Setup(level string) {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	slog.SetDefault(slog.New(handler))
}
