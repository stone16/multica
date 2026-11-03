//go:build claude_tui_acceptance && (darwin || linux)

package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
		started := time.Now()
		result, messages := runRealClaudeTUI(t, backend, "Use Bash to run `sleep 41`, then answer exactly LONG_OK.", ExecOptions{Model: "claude-haiku-5-5", Timeout: 90 * time.Second})
		if result.Status != "completed" || !strings.Contains(result.Output, "LONG_OK") || !hasToolUse(messages) || time.Since(started) < 41*time.Second {
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
		if gotTools == 0 || gotTools != blocks.toolUses || !strings.Contains(gotText.String(), blocks.text) {
			t.Fatalf("message stream mismatch: tools=%d/%d text=%q want=%q", gotTools, blocks.toolUses, gotText.String(), blocks.text)
		}
		if result.Usage[wantUsage.model] != wantUsage.usage {
			t.Fatalf("usage mismatch: result=%+v transcript=%+v", result.Usage, wantUsage)
		}
	})

	t.Run("screen-phrase-in-healthy-output", func(t *testing.T) {
		result, _ := runRealClaudeTUI(t, backend, "Use Bash to run `printf 'Do you want to proceed?'`, then answer exactly Do you want to proceed?", ExecOptions{Model: "claude-haiku-5-5", Timeout: 90 * time.Second})
		if result.Status != "completed" || !strings.Contains(result.Output, "Do you want to proceed?") {
			t.Fatalf("healthy phrase result = %+v", result)
		}
	})

	t.Run("resume-delta", func(t *testing.T) {
		first, _ := runRealClaudeTUI(t, backend, "Answer exactly FIRST_OK.", ExecOptions{Model: "claude-haiku-5-5", Timeout: 60 * time.Second})
		if first.Status != "completed" || first.SessionID == "" {
			t.Fatalf("first result = %+v", first)
		}
		_, firstUsage := transcriptForResult(t, backend.cfg.Env["HOME"], first.SessionID)
		second, _ := runRealClaudeTUI(t, backend, "Answer exactly SECOND_OK.", ExecOptions{Model: "claude-haiku-5-5", ResumeSessionID: first.SessionID, Timeout: 60 * time.Second})
		if second.Status != "completed" || !strings.Contains(second.Output, "SECOND_OK") {
			t.Fatalf("resume result = %+v", second)
		}
		if second.SessionID != first.SessionID {
			t.Fatalf("resume session = %q, want %q", second.SessionID, first.SessionID)
		}
		_, cumulative := transcriptForResult(t, backend.cfg.Env["HOME"], second.SessionID)
		want := subtractTokenUsage(cumulative.usage, firstUsage.usage)
		if second.Usage[cumulative.model] != want {
			t.Fatalf("resume usage = %+v, want delta %+v", second.Usage[cumulative.model], want)
		}
	})

	t.Run("cancel-and-decoy", func(t *testing.T) {
		for _, owned := range []bool{false, true} {
			t.Run(fmt.Sprintf("owned=%t", owned), func(t *testing.T) {
				cwd := t.TempDir()
				decoy := exec.Command("/bin/sleep", "123")
				decoy.Dir = cwd
				if err := decoy.Start(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = decoy.Process.Kill(); _ = decoy.Wait() })
				result, messages := runRealClaudeCancel(t, backend, "Use Bash to run `sleep 127 & echo $! > cancel-tool.pid; wait`, then answer CANCEL_OK.", ExecOptions{Model: "claude-haiku-5-5", Cwd: cwd, CwdOwnedByDaemon: owned, Timeout: 5 * time.Minute})
				if result.Status != "cancelled" || !hasToolUse(messages) {
					t.Fatalf("cancel result = %+v messages=%+v", result, messages)
				}
				if state := processState(t, decoy.Process.Pid); state == "" || strings.HasPrefix(state, "Z") {
					t.Fatalf("decoy state after cancellation = %q", state)
				}
			})
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

	t.Run("write-ask-rule-fails-closed", func(t *testing.T) {
		cwd := t.TempDir()
		settings := filepath.Join(t.TempDir(), "settings.json")
		if err := os.WriteFile(settings, []byte(`{"permissions":{"ask":["Write"]}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		result, messages := runRealClaudeTUI(t, backend, "Use the Write tool to create ask-rule.txt containing exactly ASK_OK, then answer exactly WRITE_OK. Do not use Bash or any other tool.", ExecOptions{Model: "claude-haiku-5-5", Cwd: cwd, ClaudeSettingsPath: settings, Timeout: 30 * time.Second})
		if result.Status != "failed" || !strings.Contains(result.Error, "interactive confirmation") || time.Since(started) > 15*time.Second {
			t.Fatalf("Write ask-rule result after %s = %+v", time.Since(started), result)
		}
		var sawWrite bool
		for _, message := range messages {
			if message.Type == MessageToolUse && message.Tool == "Write" {
				sawWrite = true
			}
		}
		if !sawWrite {
			t.Fatal("Write ask rule was not exercised")
		}
		if _, err := os.Stat(filepath.Join(cwd, "ask-rule.txt")); !os.IsNotExist(err) {
			t.Fatalf("unapproved Write file stat = %v, want not exist", err)
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

func runRealClaudeCancel(t *testing.T, backend *claudeTUIBackend, prompt string, opts ExecOptions) (Result, []Message) {
	t.Helper()
	if opts.Cwd == "" {
		opts.Cwd = t.TempDir()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session, err := backend.Execute(ctx, prompt, opts)
	if err != nil {
		t.Fatalf("real Claude execute: %v", err)
	}
	var messages []Message
	messagesCh := session.Messages
	var result Result
	toolStarted := false
	toolPID := 0
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(90 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case message, ok := <-messagesCh:
			if !ok {
				messagesCh = nil
				continue
			}
			messages = append(messages, message)
			if message.Type == MessageToolUse && !toolStarted {
				toolStarted = true
			}
		case <-ticker.C:
			if toolStarted && toolPID == 0 {
				data, err := os.ReadFile(filepath.Join(opts.Cwd, "cancel-tool.pid"))
				if err != nil {
					continue
				}
				pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
				if err != nil || pid <= 0 {
					continue
				}
				if state := processState(t, pid); state == "" || strings.HasPrefix(state, "Z") {
					t.Fatalf("tool pid %d was not alive before cancel: %q", pid, state)
				}
				toolPID = pid
				t.Cleanup(func() { _ = exec.Command("kill", "-KILL", strconv.Itoa(pid)).Run() })
				t.Logf("transcript tool_use received and tool pid %d alive before cancel", pid)
				cancel()
			}
		case result = <-session.Result:
			if !toolStarted || toolPID == 0 {
				t.Fatal("cancel result arrived before transcript tool_use and a live tool pid")
			}
			if state := processState(t, toolPID); state != "" && !strings.HasPrefix(state, "Z") {
				t.Fatalf("tool pid %d survived cancellation: %q", toolPID, state)
			}
			return result, messages
		case <-deadline.C:
			t.Fatal("cancel acceptance timed out")
		}
	}
}

func hasToolUse(messages []Message) bool {
	for _, message := range messages {
		if message.Type == MessageToolUse {
			return true
		}
	}
	return false
}

func subtractTokenUsage(total, base TokenUsage) TokenUsage {
	return TokenUsage{
		InputTokens:      total.InputTokens - base.InputTokens,
		OutputTokens:     total.OutputTokens - base.OutputTokens,
		CacheReadTokens:  total.CacheReadTokens - base.CacheReadTokens,
		CacheWriteTokens: total.CacheWriteTokens - base.CacheWriteTokens,
		CostUSDTicks:     total.CostUSDTicks - base.CostUSDTicks,
	}
}

func processState(t *testing.T, pid int) string {
	t.Helper()
	output, err := exec.Command("ps", "-o", "stat=", "-p", fmt.Sprint(pid)).Output()
	if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 1 {
		return ""
	}
	if err != nil {
		t.Fatalf("inspect process %d: %v", pid, err)
	}
	return strings.TrimSpace(string(output))
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
