package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"strconv"
	"syscall"
	"time"
)

var bridgeLogger = slog.With("component", "bridge")

func main() {
	initLogging()

	cfg, err := LoadConfig()
	if err != nil {
		bridgeLogger.Error("Failed to load config", "error", err)
		os.Exit(1)
	}

	if !cfg.Enabled {
		bridgeLogger.Info("Disabled in settings.json (telegramBridge.enabled = false). Exiting.")
		os.Exit(0)
	}

	// Look up unprivileged user for Claude subprocess isolation
	var claudeCredential *syscall.Credential
	claudeUser := os.Getenv("CLAUDE_RUN_AS_USER")
	if claudeUser == "" {
		claudeUser = "pai"
	}
	if u, err := user.Lookup(claudeUser); err == nil {
		uid, _ := strconv.ParseUint(u.Uid, 10, 32)
		gid, _ := strconv.ParseUint(u.Gid, 10, 32)
		claudeCredential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
		bridgeLogger.Info("Claude subprocesses configured", "claude_user", claudeUser, "uid", uid, "gid", gid)
	} else {
		bridgeLogger.Warn("Claude run-as user not found, using current user", "claude_user", claudeUser, "error", err)
	}

	// Memory manager
	memory := NewMemoryManager(cfg)
	bridgeLogger.Info("Memory logging configured", "enabled", cfg.Memory.Enabled, "path", cfg.Memory.BasePath)

	// Session manager
	sessions := NewSessionManager(cfg, memory, claudeCredential)

	// Ralph task manager (optional — fails gracefully if no DB configured)
	var ralph *RalphManager
	ralph, err = NewRalphManager(cfg, claudeCredential)
	if err != nil {
		bridgeLogger.Info("Ralph disabled", "reason", err.Error())
	} else {
		defer ralph.Close()
	}

	// Telegram bot
	elevenLabsKey := os.Getenv("ELEVENLABS_API_KEY")
	bot, err := NewBot(cfg, sessions, ralph, elevenLabsKey)
	if err != nil {
		bridgeLogger.Error("Failed to create bot", "error", err)
		os.Exit(1)
	}

	// Wire bot reference back to Ralph for Telegram notifications
	if ralph != nil {
		ralph.SetBot(bot)
	}

	// Health check server
	mux := http.NewServeMux()
	startTime := time.Now()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		pollAgo := bot.LastPollSecondsAgo()
		status := "ok"
		if pollAgo < 0 || pollAgo > 120 {
			status = "degraded"
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":                status,
			"service":               "pai-telegram-bridge",
			"uptime":                time.Since(startTime).Seconds(),
			"last_poll_seconds_ago": pollAgo,
			"timestamp":             time.Now().UTC().Format(time.RFC3339),
		})
	})

	go func() {
		addr := fmt.Sprintf("127.0.0.1:%d", cfg.Server.Port)
		bridgeLogger.Info("Health server listening", "addr", addr)
		if err := http.ListenAndServe(addr, mux); err != nil {
			bridgeLogger.Error("Health server error", "error", err)
		}
	}()

	// Stale session cleanup + rate limiter pruning
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			sessions.CleanStale()
			bot.cleanRateMap()
		}
	}()

	// Graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		bridgeLogger.Info("Shutting down...")
		bot.Stop()
		sessions.FlushAll()
		os.Exit(0)
	}()

	// Start bot (blocking)
	bridgeLogger.Info("Starting bot with long-polling...")
	bot.Start()
}
