//go:build claude_tui_acceptance && (darwin || linux)

package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
)

// This suite intentionally requires an explicit opt-in. It uses the operator's
// already-authenticated Claude installation and never starts a Multica server.
func TestClaudeTUIRealAcceptance(t *testing.T) {
	if os.Getenv("MULTICA_RUN_REAL_CLAUDE_TUI_ACCEPTANCE") != "1" {
		t.Fatal("real Claude acceptance disabled; set MULTICA_RUN_REAL_CLAUDE_TUI_ACCEPTANCE=1")
	}
	path := os.Getenv("MULTICA_CLAUDE_PATH")
	if path == "" {
		var err error
		path, err = exec.LookPath("claude")
		if err != nil {
			t.Fatalf("real claude not found: %v", err)
		}
	}
	home := os.Getenv("MULTICA_ACCEPTANCE_HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	backend := &claudeTUIBackend{cfg: Config{ClaudeMode: "tui", ExecutablePath: path, Env: map[string]string{"HOME": home}}}

	t.Run("long-turn", func(t *testing.T) {
		result, _ := runRealClaudeTUI(t, backend, "Use Bash to run `sleep 41`, then answer exactly LONG_OK.", ExecOptions{Model: "claude-haiku-5-5", Timeout: 90 * time.Second})
		if result.Status != "completed" || !strings.Contains(result.Output, "LONG_OK") {
			t.Fatalf("long turn result = %+v", result)
		}
	})

	t.Run("message-output-and-usage", func(t *testing.T) {
		result, messages := runRealClaudeTUI(t, backend, "Use Bash to run `printf TOOL_OK`, then answer exactly FINAL_OK.", ExecOptions{Model: "claude-haiku-5-5", Timeout: 90 * time.Second})
		if result.Status != "completed" || !strings.Contains(result.Output, "FINAL_OK") {
			t.Fatalf("multi-tool result = %+v", result)
		}
		blocks, wantUsage := transcriptForResult(t, backend.cfg.Env["HOME"], result.SessionID)
		var gotText strings.Builder
		var gotTools int
		for _, message := range messages {
			switch message.Type {
			case MessageText:
				gotText.WriteString(message.Content)
			case MessageToolUse:
				gotTools++
			}
		}
		if gotTools != blocks.toolUses || !strings.Contains(gotText.String(), blocks.text) {
			t.Fatalf("message stream mismatch: tools=%d/%d text=%q want=%q", gotTools, blocks.toolUses, gotText.String(), blocks.text)
		}
		if result.Usage[wantUsage.model] != wantUsage.usage {
			t.Fatalf("usage mismatch: result=%+v transcript=%+v", result.Usage, wantUsage)
		}
	})

	t.Run("resume-delta", func(t *testing.T) {
		first, _ := runRealClaudeTUI(t, backend, "Answer exactly FIRST_OK.", ExecOptions{Model: "claude-haiku-5-5", Timeout: 60 * time.Second})
		if first.Status != "completed" || first.SessionID == "" {
			t.Fatalf("first result = %+v", first)
		}
		second, _ := runRealClaudeTUI(t, backend, "Answer exactly SECOND_OK.", ExecOptions{Model: "claude-haiku-5-5", ResumeSessionID: first.SessionID, Timeout: 60 * time.Second})
		if second.Status != "completed" || !strings.Contains(second.Output, "SECOND_OK") {
			t.Fatalf("resume result = %+v", second)
		}
	})

	t.Run("cancel-and-decoy", func(t *testing.T) {
		cwd := t.TempDir()
		decoy := exec.Command("/bin/sleep", "120")
		decoy.Dir = cwd
		if err := decoy.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = decoy.Process.Kill(); _ = decoy.Wait() }()
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		result, _ := runRealClaudeTUIContext(t, backend, ctx, "Use Bash to run `sleep 120 & wait`, then answer CANCEL_OK.", ExecOptions{Model: "claude-haiku-5-5", Cwd: cwd, CwdOwnedByDaemon: false, Timeout: 5 * time.Minute})
		if result.Status != "cancelled" && result.Status != "timeout" {
			t.Fatalf("cancel result = %+v", result)
		}
		if err := decoy.Process.Signal(syscall.Signal(0)); err != nil {
			t.Fatalf("decoy process was harmed: %v", err)
		}
		owned := t.TempDir()
		ownedCtx, ownedCancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer ownedCancel()
		ownedResult, _ := runRealClaudeTUIContext(t, backend, ownedCtx, "Use Bash to run `sleep 120 & wait`, then answer OWNED_CANCEL_OK.", ExecOptions{Model: "claude-haiku-5-5", Cwd: owned, CwdOwnedByDaemon: true, Timeout: 5 * time.Minute})
		if ownedResult.Status != "cancelled" && ownedResult.Status != "timeout" {
			t.Fatalf("daemon-owned cancel result = %+v", ownedResult)
		}
	})

	t.Run("unknown-modal-fails-closed", func(t *testing.T) {
		hookDir := t.TempDir()
		hook := filepath.Join(hookDir, "ask.sh")
		if err := os.WriteFile(hook, []byte("#!/bin/sh\nprintf '%s' '{\"hookSpecificOutput\":{\"hookEventName\":\"PreToolUse\",\"permissionDecision\":\"ask\"}}'\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		settings := filepath.Join(hookDir, "settings.json")
		if err := os.WriteFile(settings, []byte(`{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"`+hook+`"}]}]}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		result, _ := runRealClaudeTUI(t, backend, "Use Bash to run `sleep 120`, then answer MODAL_OK.", ExecOptions{Model: "claude-haiku-5-5", ClaudeSettingsPath: settings, Timeout: 45 * time.Second})
		if result.Status != "failed" || !strings.Contains(result.Error, "interactive confirmation") {
			t.Fatalf("modal result = %+v", result)
		}
	})

	t.Run("refused-resume", func(t *testing.T) {
		result, _ := runRealClaudeTUI(t, backend, "Answer exactly NEVER.", ExecOptions{Model: "claude-haiku-5-5", ResumeSessionID: uuid.NewString(), Timeout: 45 * time.Second})
		if result.Status != "failed" || !result.ResumeRejected || result.SessionID != "" {
			t.Fatalf("refused resume result = %+v", result)
		}
	})
}

func runRealClaudeTUI(t *testing.T, backend *claudeTUIBackend, prompt string, opts ExecOptions) (Result, []Message) {
	return runRealClaudeTUIContext(t, backend, context.Background(), prompt, opts)
}

func runRealClaudeTUIContext(t *testing.T, backend *claudeTUIBackend, ctx context.Context, prompt string, opts ExecOptions) (Result, []Message) {
	t.Helper()
	if opts.Cwd == "" {
		opts.Cwd = t.TempDir()
	}
	session, err := backend.Execute(ctx, prompt, opts)
	if err != nil {
		t.Fatalf("real Claude execute: %v", err)
	}
	var result Result
	select {
	case result = <-session.Result:
	case <-time.After(opts.Timeout + 30*time.Second):
		t.Fatal("real Claude acceptance timed out waiting for Result")
	}
	var messages []Message
	for message := range session.Messages {
		messages = append(messages, message)
	}
	return result, messages
}

type acceptanceBlocks struct {
	text     string
	toolUses int
}

type acceptanceUsage struct {
	model string
	usage TokenUsage
}

func transcriptForResult(t *testing.T, home, sessionID string) (acceptanceBlocks, acceptanceUsage) {
	t.Helper()
	cfgDir, err := claudeConfigDir([]string{"HOME=" + home}, "")
	if err != nil {
		t.Fatal(err)
	}
	paths, _ := filepath.Glob(filepath.Join(cfgDir, "projects", "*", sessionID+".jsonl"))
	if len(paths) != 1 {
		t.Fatalf("transcript for session %s: %v", sessionID, paths)
	}
	f, err := os.Open(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	seen := map[string]struct{}{}
	var out acceptanceBlocks
	var u acceptanceUsage
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), 64<<20)
	for scanner.Scan() {
		var row map[string]any
		if json.Unmarshal(scanner.Bytes(), &row) != nil || row["isSidechain"] == true {
			continue
		}
		if row["type"] == "cost-state" {
			models, _ := row["modelUsage"].(map[string]any)
			for model, raw := range models {
				value, _ := raw.(map[string]any)
				u = acceptanceUsage{model: model, usage: TokenUsage{InputTokens: int64(number(value["inputTokens"])), OutputTokens: int64(number(value["outputTokens"])), CacheReadTokens: int64(number(value["cacheReadInputTokens"])), CacheWriteTokens: int64(number(value["cacheCreationInputTokens"]))}}
			}
			continue
		}
		if row["type"] != "assistant" {
			continue
		}
		message, _ := row["message"].(map[string]any)
		id, _ := message["id"].(string)
		usageNew := id == ""
		if id != "" {
			_, usageNew = seen[id]
			usageNew = !usageNew
			seen[id] = struct{}{}
		}
		model, _ := message["model"].(string)
		if usage, ok := message["usage"].(map[string]any); ok && model != "" && usageNew {
			u.model = model
			u.usage.InputTokens += int64(number(usage["input_tokens"]))
			u.usage.OutputTokens += int64(number(usage["output_tokens"]))
			u.usage.CacheReadTokens += int64(number(usage["cache_read_input_tokens"]))
			u.usage.CacheWriteTokens += int64(number(usage["cache_creation_input_tokens"]))
		}
		blocks, _ := message["content"].([]any)
		for _, raw := range blocks {
			block, _ := raw.(map[string]any)
			switch block["type"] {
			case "text":
				text, _ := block["text"].(string)
				out.text += text
			case "tool_use":
				out.toolUses++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if out.text == "" {
		t.Fatal("transcript had no assistant text")
	}
	return out, u
}
