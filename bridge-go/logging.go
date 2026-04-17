package main

import (
	"log/slog"
	"os"
)

// initLogging configures slog as the default logger with a JSON handler.
//
// Why JSON: container stdout flows through fluent-bit's kubernetes filter
// with Merge_Log On + Keep_Log Off. JSON-formatted log lines have their
// keys promoted to top-level OpenSearch document fields, making them
// queryable and aggregatable. The legacy stdlib log package emits opaque
// text strings that end up as a single `log` field blob.
//
// Each file in this package declares a package-level logger var like:
//
//	var memoryLogger = slog.With("component", "memory")
//
// Then call sites use idiomatic key-value pairs:
//
//	memoryLogger.Info("Flushing session", "session_id", sid, "user_id", uid)
//
// LOG_LEVEL env var ("debug", "info", "warn", "error") sets the threshold.
// Defaults to "info".
func initLogging() {
	level := slog.LevelInfo
	switch os.Getenv("LOG_LEVEL") {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	})
	logger := slog.New(handler).With("service", "pai-bridge")
	slog.SetDefault(logger)
}
