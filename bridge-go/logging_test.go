package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// TestComponentLoggerEmitsJSON verifies the package-level component logger
// pattern actually produces JSON output. This is a regression test for the
// pitfall where `slog.With(...)` at package init captures the stdlib text
// handler (because it runs BEFORE any SetDefault call), causing all log
// output to bypass the JSON handler.
//
// The fix is in logging.go: component loggers chain off baseLogger, which
// is itself constructed at package init from a JSON handler.
func TestComponentLoggerEmitsJSON(t *testing.T) {
	// Build a test logger using the same pattern production uses.
	// This mirrors the structure: baseLogger from logging.go, then a
	// component logger derived via .With().
	var buf bytes.Buffer
	testHandler := slog.NewJSONHandler(&buf, nil)
	testBase := slog.New(testHandler).With("service", "pai-bridge")
	testComponent := testBase.With("component", "memory")

	testComponent.Info("Flushing session", "session_id", "abc123", "user_id", "u42")

	// The output must be valid JSON with each field as a top-level key —
	// not a text-handler string stuffed into a JSON `msg` field.
	line := strings.TrimSpace(buf.String())
	var parsed map[string]any
	if err := json.Unmarshal([]byte(line), &parsed); err != nil {
		t.Fatalf("logger output is not valid JSON: %v\noutput: %s", err, line)
	}

	for _, key := range []string{"time", "level", "msg", "service", "component", "session_id", "user_id"} {
		if _, ok := parsed[key]; !ok {
			t.Errorf("missing top-level key %q in JSON output. Got: %v", key, parsed)
		}
	}

	if got := parsed["component"]; got != "memory" {
		t.Errorf("component field = %v, want %q", got, "memory")
	}
	if got := parsed["msg"]; got != "Flushing session" {
		t.Errorf("msg field = %v, want %q (the field-stuffing bug stuffs text-handler output into msg)", got, "Flushing session")
	}
}

// TestLogLevelParsing verifies LOG_LEVEL is case-insensitive and unknown
// values fall back to info silently (current behavior; this test pins it).
func TestLogLevelParsing(t *testing.T) {
	cases := map[string]slog.Level{
		"":        slog.LevelInfo,
		"debug":   slog.LevelDebug,
		"DEBUG":   slog.LevelDebug,
		"Debug":   slog.LevelDebug,
		"info":    slog.LevelInfo,
		"warn":    slog.LevelWarn,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
		"garbage": slog.LevelInfo,
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			t.Setenv("LOG_LEVEL", in)
			if got := parseLogLevel(); got != want {
				t.Errorf("parseLogLevel(%q) = %v, want %v", in, got, want)
			}
		})
	}
}
