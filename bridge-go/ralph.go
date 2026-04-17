package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/lib/pq"
)

var ralphLogger = slog.With("component", "ralph")

// RalphDirective is the parsed JSON from a RALPH: directive in Claude output.
type RalphDirective struct {
	Title         string   `json:"title"`
	SpecFile      string   `json:"spec_file"`
	Workspace     string   `json:"workspace,omitempty"`
	Branch        string   `json:"branch,omitempty"`
	Tags          []string `json:"tags,omitempty"`
	MaxIterations int      `json:"max_iterations,omitempty"`
}

// RalphManager manages autonomous Ralph task loops.
type RalphManager struct {
	db               *sql.DB
	config           *Config
	bot              *Bot
	claudeCredential *syscall.Credential
	mu               sync.Mutex
	activeCount      int
}

func NewRalphManager(cfg *Config, cred *syscall.Credential) (*RalphManager, error) {
	dbURL := cfg.Ralph.DatabaseURL
	if dbURL == "" {
		dbURL = os.Getenv("PAI_DATABASE_URL")
	}
	if dbURL == "" {
		return nil, fmt.Errorf("ralph: no database URL configured (set telegramBridge.ralph.database_url or DATABASE_URL env)")
	}

	if !strings.Contains(dbURL, "sslmode=") {
		sep := "?"
		if strings.Contains(dbURL, "?") {
			sep = "&"
		}
		dbURL += sep + "sslmode=require"
	}

	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		return nil, fmt.Errorf("ralph: database open: %w", err)
	}
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ralph: database ping: %w", err)
	}

	ralphLogger.Info("Database connected")
	return &RalphManager{
		db:               db,
		config:           cfg,
		claudeCredential: cred,
	}, nil
}

func (rm *RalphManager) SetBot(bot *Bot) { rm.bot = bot }
func (rm *RalphManager) Close() {
	if rm.db != nil {
		rm.db.Close()
	}
}

// StartRalph creates a new Ralph task and starts the autonomous loop.
func (rm *RalphManager) StartRalph(chatID int64, directive *RalphDirective) (int, error) {
	rm.mu.Lock()
	if rm.activeCount >= rm.config.Ralph.MaxConcurrent {
		rm.mu.Unlock()
		return 0, fmt.Errorf("max concurrent Ralphs reached (%d)", rm.config.Ralph.MaxConcurrent)
	}
	rm.activeCount++
	rm.mu.Unlock()

	maxIter := directive.MaxIterations
	if maxIter <= 0 {
		maxIter = rm.config.Ralph.DefaultMaxIterations
	}

	spec, err := os.ReadFile(directive.SpecFile)
	if err != nil {
		rm.mu.Lock()
		rm.activeCount--
		rm.mu.Unlock()
		return 0, fmt.Errorf("read spec file %s: %w", directive.SpecFile, err)
	}

	var id int
	err = rm.db.QueryRow(`
		INSERT INTO pai.ralphs (status, title, spec, tags, workspace, branch, max_iterations, source_message)
		VALUES ('queued', $1, $2, $3, $4, $5, $6, $7)
		RETURNING id`,
		directive.Title,
		string(spec),
		pqTextArray(directive.Tags),
		nullStr(directive.Workspace),
		nullStr(directive.Branch),
		maxIter,
		nullStr(directive.SpecFile),
	).Scan(&id)
	if err != nil {
		rm.mu.Lock()
		rm.activeCount--
		rm.mu.Unlock()
		return 0, fmt.Errorf("insert ralph: %w", err)
	}

	ralphLogger.Info("Task created", "task_id", id, "title", directive.Title, "max_iterations", maxIter)
	go rm.runLoop(id, chatID, directive, string(spec), maxIter)
	return id, nil
}

func (rm *RalphManager) runLoop(id int, chatID int64, directive *RalphDirective, spec string, maxIterations int) {
	defer func() {
		rm.mu.Lock()
		rm.activeCount--
		rm.mu.Unlock()
	}()

	rm.setStatus(id, "active")
	rm.db.Exec("UPDATE pai.ralphs SET started_at = now() WHERE id = $1", id)

	// Create git branch if applicable
	if directive.Workspace != "" && directive.Branch != "" {
		if err := gitCheckoutBranch(directive.Workspace, directive.Branch); err != nil {
			ralphLogger.Warn("Branch setup failed, continuing anyway", "task_id", id, "error", err)
		}
	}

	// Working directory for this Ralph's state files
	progressDir := fmt.Sprintf("/tmp/ralph-%d", id)
	os.MkdirAll(progressDir, 0755)
	specPath := filepath.Join(progressDir, "SPEC.md")
	progressPath := filepath.Join(progressDir, "PROGRESS.md")
	os.WriteFile(specPath, []byte(spec), 0644)
	os.WriteFile(progressPath, []byte("# Progress\n\nNo iterations completed yet.\n"), 0644)

	notifyEvery := rm.config.Ralph.NotificationInterval
	if notifyEvery <= 0 {
		notifyEvery = 3
	}

	for i := 1; i <= maxIterations; i++ {
		if rm.isCancelled(id) {
			ralphLogger.Info("Task cancelled", "task_id", id)
			rm.setStatus(id, "cancelled")
			rm.db.Exec("UPDATE pai.ralphs SET finished_at = now() WHERE id = $1", id)
			rm.notify(chatID, fmt.Sprintf("Ralph #%d cancelled: %s", id, directive.Title))
			return
		}

		ralphLogger.Info("Iteration starting", "task_id", id, "iteration", i, "max_iterations", maxIterations)

		progress, _ := os.ReadFile(progressPath)
		prompt := buildRalphPrompt(spec, string(progress), directive, i, maxIterations)

		output, err := rm.runClaude(directive, prompt)
		if err != nil {
			errMsg := err.Error()
			ralphLogger.Error("Iteration failed", "task_id", id, "iteration", i, "error", err)
			rm.db.Exec(
				`UPDATE pai.ralphs SET iterations = $1, error = $2, status = 'failed', finished_at = now() WHERE id = $3`,
				i, errMsg, id,
			)
			rm.notify(chatID, fmt.Sprintf("Ralph #%d failed (iteration %d): %s\n\nError: %s", id, i, directive.Title, truncate(errMsg, 200)))
			return
		}

		progressText, artifacts, complete, blocked := parseRalphOutput(output)

		// Update progress file for next iteration
		if progressText != "" {
			newProgress := fmt.Sprintf("# Progress\n\nIteration %d/%d completed.\n\n%s\n", i, maxIterations, progressText)
			os.WriteFile(progressPath, []byte(newProgress), 0644)
		}

		rm.updateIteration(id, i, progressText, artifacts)

		if blocked != "" {
			ralphLogger.Warn("Task blocked", "task_id", id, "reason", blocked)
			rm.db.Exec(
				`UPDATE pai.ralphs SET iterations = $1, error = $2, status = 'failed', finished_at = now() WHERE id = $3`,
				i, "blocked: "+blocked, id,
			)
			rm.notify(chatID, fmt.Sprintf("Ralph #%d blocked: %s\n\nReason: %s", id, directive.Title, blocked))
			return
		}

		// Send periodic or completion notifications
		if i%notifyEvery == 0 || complete {
			status := fmt.Sprintf("Ralph #%d — iteration %d/%d: %s", id, i, maxIterations, directive.Title)
			if progressText != "" {
				status += "\n" + progressText
			}
			rm.notify(chatID, status)
		}

		if complete {
			ralphLogger.Info("Task completed", "task_id", id, "iterations", i)
			rm.db.Exec("UPDATE pai.ralphs SET status = 'completed', iterations = $1, finished_at = now() WHERE id = $2", i, id)
			rm.notify(chatID, fmt.Sprintf("Ralph #%d complete: %s (%d iterations)", id, directive.Title, i))
			return
		}
	}

	// Max iterations reached without completion
	ralphLogger.Warn("Max iterations reached", "task_id", id, "max_iterations", maxIterations)
	rm.db.Exec(`UPDATE pai.ralphs SET status = 'failed', error = 'max iterations reached', finished_at = now() WHERE id = $1`, id)
	rm.notify(chatID, fmt.Sprintf("Ralph #%d stopped: %s — hit max iterations (%d)", id, directive.Title, maxIterations))
}

func buildRalphPrompt(spec, progress string, directive *RalphDirective, iteration, maxIterations int) string {
	var sb strings.Builder
	sb.WriteString("[RALPH AUTONOMOUS TASK LOOP]\n")
	sb.WriteString(fmt.Sprintf("Iteration %d of %d.\n", iteration, maxIterations))
	sb.WriteString("Read the spec, review progress from prior iterations, pick the next incomplete task, and execute it.\n\n")
	sb.WriteString("OUTPUT THESE SIGNALS on their own line when appropriate:\n")
	sb.WriteString("- RALPH_PROGRESS: <what you accomplished this iteration>\n")
	sb.WriteString("- RALPH_COMPLETE: <summary> — when ALL spec items are done\n")
	sb.WriteString("- RALPH_BLOCKED: <reason> — if you cannot proceed without human input\n")
	sb.WriteString("- RALPH_ARTIFACT: <type>:<value> — record outputs (commit:<sha>, file:<path>, doc:<id>, pr:<url>)\n\n")

	if directive.Workspace != "" {
		sb.WriteString(fmt.Sprintf("Workspace: %s\n", directive.Workspace))
	}
	if directive.Branch != "" {
		sb.WriteString(fmt.Sprintf("Branch: %s\n", directive.Branch))
	}

	sb.WriteString("\n--- SPEC ---\n")
	sb.WriteString(spec)
	sb.WriteString("\n--- END SPEC ---\n\n")
	sb.WriteString("--- PROGRESS FROM PRIOR ITERATIONS ---\n")
	sb.WriteString(progress)
	sb.WriteString("\n--- END PROGRESS ---\n")
	return sb.String()
}

func (rm *RalphManager) runClaude(directive *RalphDirective, prompt string) (string, error) {
	claudePath := os.Getenv("CLAUDE_PATH")
	if claudePath == "" {
		home, _ := os.UserHomeDir()
		claudePath = filepath.Join(home, ".local/bin/claude")
	}
	if resolved, err := filepath.EvalSymlinks(claudePath); err == nil {
		claudePath = resolved
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	args := []string{
		"-p", prompt,
		"--output-format", "stream-json",
		"--verbose",
		"--model", rm.config.Sessions.DefaultModel,
		"--setting-sources", "user", // Block project-level configs in untrusted workspaces (CVE-2026-21852)
	}

	cmd := exec.CommandContext(ctx, claudePath, args...)

	if directive.Workspace != "" {
		cmd.Dir = directive.Workspace
	} else {
		cmd.Dir = rm.config.Sessions.DefaultWorkDir
	}

	// Build env — same pattern as session.go, strip secrets Claude doesn't need
	env := os.Environ()
	if rm.claudeCredential != nil {
		claudeHome := os.Getenv("CLAUDE_USER_HOME")
		if claudeHome == "" {
			claudeHome = "/home/pai"
		}
		paiDir := filepath.Join(claudeHome, ".claude")
		filtered := make([]string, 0, len(env))
		for _, e := range env {
			if strings.HasPrefix(e, "HOME=") ||
				strings.HasPrefix(e, "PAI_DIR=") ||
				strings.HasPrefix(e, "TELEGRAM_BOT_TOKEN=") ||
				strings.HasPrefix(e, "ELEVENLABS_API_KEY=") ||
				strings.HasPrefix(e, "PAI_DATABASE_URL=") {
				continue
			}
			filtered = append(filtered, e)
		}
		env = append(filtered, "HOME="+claudeHome, "PAI_DIR="+paiDir)
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Credential: rm.claudeCredential,
		}
	}
	cmd.Env = env

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	if err := cmd.Run(); err != nil {
		stderr := strings.TrimSpace(stderrBuf.String())
		if stderr != "" {
			return "", fmt.Errorf("claude exited: %s", stderr)
		}
		return "", fmt.Errorf("claude subprocess failed: %w", err)
	}

	// Parse stream-json to extract text
	var fullText strings.Builder
	for _, line := range strings.Split(stdoutBuf.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var event map[string]interface{}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		if chunk := extractTextFromEvent(event); chunk != "" {
			fullText.WriteString(chunk)
		}
	}

	return fullText.String(), nil
}

// parseRalphOutput extracts progress, artifacts, and completion/blocked signals from Claude output.
func parseRalphOutput(output string) (progress string, artifacts map[string][]string, complete bool, blocked string) {
	artifacts = make(map[string][]string)
	var progressLines []string

	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "RALPH_COMPLETE:") {
			complete = true
			progress = strings.TrimSpace(strings.TrimPrefix(trimmed, "RALPH_COMPLETE:"))
			return
		}
		if strings.HasPrefix(trimmed, "RALPH_BLOCKED:") {
			blocked = strings.TrimSpace(strings.TrimPrefix(trimmed, "RALPH_BLOCKED:"))
			return
		}
		if strings.HasPrefix(trimmed, "RALPH_PROGRESS:") {
			progressLines = append(progressLines, strings.TrimSpace(strings.TrimPrefix(trimmed, "RALPH_PROGRESS:")))
		}
		if strings.HasPrefix(trimmed, "RALPH_ARTIFACT:") {
			rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "RALPH_ARTIFACT:"))
			if parts := strings.SplitN(rest, ":", 2); len(parts) == 2 {
				key := strings.TrimSpace(parts[0])
				val := strings.TrimSpace(parts[1])
				artifacts[key] = append(artifacts[key], val)
			}
		}
	}

	progress = strings.Join(progressLines, "\n")
	return
}

// --- DB helpers ---

func (rm *RalphManager) setStatus(id int, status string) {
	if _, err := rm.db.Exec("UPDATE pai.ralphs SET status = $1 WHERE id = $2", status, id); err != nil {
		ralphLogger.Warn("Status update failed", "task_id", id, "error", err)
	}
}

func (rm *RalphManager) updateIteration(id int, iteration int, progress string, artifacts map[string][]string) {
	artifactsJSON, _ := json.Marshal(artifacts)
	_, err := rm.db.Exec(
		`UPDATE pai.ralphs SET iterations = $1, progress = $2, artifacts = $3 WHERE id = $4`,
		iteration, progress, string(artifactsJSON), id,
	)
	if err != nil {
		ralphLogger.Warn("Iteration update failed", "task_id", id, "error", err)
	}
}

func (rm *RalphManager) isCancelled(id int) bool {
	var status string
	if err := rm.db.QueryRow("SELECT status FROM pai.ralphs WHERE id = $1", id).Scan(&status); err != nil {
		return false
	}
	return status == "cancelled"
}

func (rm *RalphManager) notify(chatID int64, text string) {
	if rm.bot != nil {
		rm.bot.send(chatID, text)
	}
}

// --- Utility helpers ---

func gitCheckoutBranch(workspace, branch string) error {
	cmd := exec.Command("git", "-C", workspace, "rev-parse", "--is-inside-work-tree")
	if err := cmd.Run(); err != nil {
		return nil // not a git repo, skip
	}
	// Try create-and-checkout; fall back to just checkout if branch exists
	cmd = exec.Command("git", "-C", workspace, "checkout", "-b", branch)
	if _, err := cmd.CombinedOutput(); err != nil {
		cmd = exec.Command("git", "-C", workspace, "checkout", branch)
		if out, err2 := cmd.CombinedOutput(); err2 != nil {
			return fmt.Errorf("checkout %s: %s", branch, string(out))
		}
	}
	return nil
}

func pqTextArray(ss []string) string {
	if len(ss) == 0 {
		return "{}"
	}
	escaped := make([]string, len(ss))
	for i, s := range ss {
		escaped[i] = `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return "{" + strings.Join(escaped, ",") + "}"
}

func nullStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
