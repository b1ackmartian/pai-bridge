package main

import (
	"log/slog"
	"os"
	"strings"
)

// JSON logging setup for pai-bridge.
//
// Why JSON: container stdout flows through fluent-bit's kubernetes filter
// with Merge_Log On + Keep_Log Off. JSON-formatted log lines have their
// keys promoted to top-level OpenSearch document fields, making them
// queryable and aggregatable. The legacy stdlib log package emits opaque
// text strings that end up as a single `log` field blob.
//
// IMPLEMENTATION NOTE — package-level eager initialization:
// The handler and base logger are constructed at package var init, BEFORE
// main() runs. Each file's component logger chains off baseLogger via
// .With("component", "<name>"). Go's package init handles the dependency
// order automatically: any var that references baseLogger blocks until
// baseLogger is initialized.
//
// We intentionally do NOT use slog.SetDefault() at runtime in main(),
// because that would not affect package-level component loggers — they
// would have already captured a reference to whatever slog.Default()
// was at THEIR init time (the stdlib text handler).
//
// LOG_LEVEL env var (debug/info/warn/error, case-insensitive) sets the
// threshold. Unknown or empty values default to info.
//
// Usage from each file:
//
//	var memoryLogger = baseLogger.With("component", "memory")
//
// Then call sites use idiomatic key-value pairs:
//
//	memoryLogger.Info("Flushing session", "session_id", sid, "user_id", uid)

var (
	logHandler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLogLevel(),
	})
	baseLogger = slog.New(logHandler).With("service", "pai-bridge")
)

// init sets baseLogger as the default so any direct slog.Info/slog.Error
// calls (e.g., from libraries) also emit JSON. Component loggers do NOT
// depend on this — they chain off baseLogger directly.
func init() {
	slog.SetDefault(baseLogger)
}

func parseLogLevel() slog.Level {
	switch strings.ToLower(os.Getenv("LOG_LEVEL")) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
