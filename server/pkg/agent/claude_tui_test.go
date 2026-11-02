//go:build darwin || linux

package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestClaudeTUIRejectedArg(t *testing.T) {
	for _, arg := range []string{"--output-format=json", "--prefill=hello", "--worktree", "-w=foo", "--include-hook-events"} {
		if !claudeTUIRejectedArg(arg) {
			t.Errorf("claudeTUIRejectedArg(%q) = false", arg)
		}
	}
	for _, arg := range []string{"--model=haiku", "--effort"} {
		if claudeTUIRejectedArg(arg) {
			t.Errorf("claudeTUIRejectedArg(%q) = true", arg)
		}
	}
}

func TestClaudeTUIEnvRemovesChildMarkers(t *testing.T) {
	env := claudeTUIEnv(map[string]string{
		"CLAUDE_CODE_CHILD_SESSION":       "1",
		"CLAUDE_CODE_MESSAGING_TOKEN":     "secret",
		"CLAUDE_CODE_SKIP_PROMPT_HISTORY": "1",
		"TUI_TEST_VALUE":                  "ok",
	})
	joined := "\n"
	for _, item := range env {
		joined += item + "\n"
	}
	for _, key := range []string{"CLAUDE_CODE_CHILD_SESSION=", "CLAUDE_CODE_MESSAGING_TOKEN=", "CLAUDE_CODE_SKIP_PROMPT_HISTORY="} {
		if containsLine(joined, key) {
			t.Errorf("child marker survived: %s", key)
		}
	}
	for _, key := range []string{"CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1", "DISABLE_AUTOUPDATER=1", "TUI_TEST_VALUE=ok"} {
		if !containsLine(joined, key) {
			t.Errorf("missing env %s", key)
		}
	}
}

func containsLine(s, want string) bool {
	for _, line := range claudeTUISplitLines(s) {
		if line == want {
			return true
		}
	}
	return false
}

func claudeTUISplitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return out
}

func TestClaudeTUIResumeRecoveryRowsDoNotSealOrEmit(t *testing.T) {
	var messages []Message
	ch := make(chan Message, 8)
	row := map[string]any{
		"type": "user",
		"message": map[string]any{"content": []any{map[string]any{
			"type": "tool_result", "tool_use_id": "tool-1", "content": "interrupted",
		}}},
	}
	backend := &claudeTUIBackend{}
	finalText := ""
	finalErr := ""
	sawResult := false
	reason := ""
	rows := 0
	backend.processTUIRow(row, ch, map[string]TokenUsage{}, map[string]TokenUsage{}, map[string]struct{}{}, &finalText, &finalErr, &sawResult, &reason, &rows, "session-1", "")
	close(ch)
	for m := range ch {
		messages = append(messages, m)
	}
	if rows != 0 || sawResult || len(messages) != 0 {
		t.Fatalf("synthetic resume row changed state: rows=%d result=%v messages=%v", rows, sawResult, messages)
	}
}

func TestClaudeTUIResultSessionClearsRefusedResume(t *testing.T) {
	opts := ExecOptions{ResumeSessionID: "old-session"}
	if got, rejected := tuiResultSession(opts, "old-session", true, 0, "claude: no conversation found"); got != "" || !rejected {
		t.Fatalf("refused resume = (%q, %v), want empty and rejected", got, rejected)
	}
	if got, rejected := tuiResultSession(opts, "old-session", true, 0, "claude tui startup timed out"); got != "" || rejected {
		t.Fatalf("unaccepted resume = (%q, %v), want empty and not rejected", got, rejected)
	}
	if got, rejected := tuiResultSession(opts, "old-session", true, 1, "provider rate limit"); got != "old-session" || rejected {
		t.Fatalf("failed resumed turn = (%q, %v), want retained session", got, rejected)
	}
}

func TestClaudeTUITranscriptTailRetainsPartialRow(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "projects", "p", "session.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	row := `{"type":"user","message":{"content":"hello"}}`
	if err := os.WriteFile(path, []byte(row), 0o600); err != nil {
		t.Fatal(err)
	}
	b := &claudeTUIBackend{}
	offsets := map[string]int64{}
	ch := make(chan Message, 4)
	finalText, finalErr := "", ""
	sawResult, reason, promptRows := false, "", 0
	if err := b.readTUITranscript(home, "session", offsets, ch, map[string]TokenUsage{}, map[string]TokenUsage{}, map[string]struct{}{}, &finalText, &finalErr, &sawResult, &reason, &promptRows, ""); err != nil {
		t.Fatal(err)
	}
	if offsets[path] != 0 || promptRows != 0 {
		t.Fatalf("partial row advanced state: offset=%d rows=%d", offsets[path], promptRows)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if err := b.readTUITranscript(home, "session", offsets, ch, map[string]TokenUsage{}, map[string]TokenUsage{}, map[string]struct{}{}, &finalText, &finalErr, &sawResult, &reason, &promptRows, ""); err != nil {
		t.Fatal(err)
	}
	if promptRows != 1 || offsets[path] != int64(len(row)+1) {
		t.Fatalf("completed row not consumed: offset=%d rows=%d", offsets[path], promptRows)
	}
}

func TestClaudeTUITranscriptTailRejectsOversizedRow(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "projects", "p", "session.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append([]byte(strings.Repeat("x", agentStreamMaxLineBytes+1)), '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	b := &claudeTUIBackend{}
	ch := make(chan Message, 1)
	finalText, finalErr := "", ""
	sawResult, reason, promptRows := false, "", 0
	err := b.readTUITranscript(home, "session", map[string]int64{}, ch, map[string]TokenUsage{}, map[string]TokenUsage{}, map[string]struct{}{}, &finalText, &finalErr, &sawResult, &reason, &promptRows, "")
	if err == nil || !strings.Contains(err.Error(), "transcript row exceeds") {
		t.Fatalf("oversized row error = %v", err)
	}
}

func TestClaudeTUIProviderErrorPreservesMessage(t *testing.T) {
	raw := map[string]any{"type": "assistant", "isApiErrorMessage": true, "error": "rate_limit_error: try again later", "errorDetails": map[string]any{"retry_after": 3}}
	if got := tuiProviderErrorText(raw); got != "rate_limit_error: try again later" {
		t.Fatalf("provider error = %q", got)
	}
}

func TestClaudeTUIMessageIDKeepsBlocksAndDeduplicatesUsage(t *testing.T) {
	backend := &claudeTUIBackend{}
	ch := make(chan Message, 8)
	finalText, finalErr, reason := "", "", ""
	sawResult, rows := false, 1
	seen := map[string]struct{}{}
	usage := map[string]TokenUsage{}
	row := map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"id":    "same",
			"model": "claude-haiku-5-5",
			"usage": map[string]any{"input_tokens": 7, "output_tokens": 3},
			"content": []any{
				map[string]any{"type": "text", "text": "before"},
				map[string]any{"type": "tool_use", "id": "tool-1", "name": "Bash", "input": map[string]any{"command": "true"}},
			},
		},
	}
	backend.processTUIRow(row, ch, usage, map[string]TokenUsage{}, seen, &finalText, &finalErr, &sawResult, &reason, &rows, "session", "")
	row["message"].(map[string]any)["content"] = []any{map[string]any{"type": "text", "text": "after"}, map[string]any{"type": "tool_use", "id": "tool-2", "name": "Read", "input": map[string]any{}}}
	backend.processTUIRow(row, ch, usage, map[string]TokenUsage{}, seen, &finalText, &finalErr, &sawResult, &reason, &rows, "session", "")
	close(ch)
	var messages []Message
	for message := range ch {
		messages = append(messages, message)
	}
	if finalText != "before\nafter" {
		t.Fatalf("final text = %q", finalText)
	}
	if usage["claude-haiku-5-5"].InputTokens != 7 || usage["claude-haiku-5-5"].OutputTokens != 3 {
		t.Fatalf("usage = %+v", usage)
	}
	var tools int
	for _, message := range messages {
		if message.Type == MessageToolUse {
			tools++
		}
	}
	if tools != 2 {
		t.Fatalf("tool messages = %d, messages = %+v", tools, messages)
	}
}

func TestClaudeTUIOutputUsesLatestAssistantMessage(t *testing.T) {
	backend := &claudeTUIBackend{}
	ch := make(chan Message, 4)
	finalText, finalErr, reason := "", "", ""
	sawResult, rows := false, 1
	seen := map[string]struct{}{}
	usage := map[string]TokenUsage{}
	row := func(id, text string) map[string]any {
		return map[string]any{"type": "assistant", "message": map[string]any{"id": id, "content": []any{map[string]any{"type": "text", "text": text}}}}
	}
	backend.processTUIRow(row("narration", "NARRATION"), ch, usage, nil, seen, &finalText, &finalErr, &sawResult, &reason, &rows, "session", "")
	backend.processTUIRow(row("final", "FINAL"), ch, usage, nil, seen, &finalText, &finalErr, &sawResult, &reason, &rows, "session", "")
	close(ch)
	if finalText != "FINAL" {
		t.Fatalf("final text = %q, want latest assistant message", finalText)
	}
}

func TestClaudeTUISettingsMergePreservesDeny(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"permissions":{"deny":["Skill(secret)"]},"skillOverrides":{"secret":"off"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	merged, err := writeClaudeTUISettings(path)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(merged)
	data, err := os.ReadFile(merged)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	permissions := settings["permissions"].(map[string]any)
	deny := permissions["deny"].([]any)
	if len(deny) != 4 || deny[0] != "Skill(secret)" || !settings["skipDangerousModePermissionPrompt"].(bool) {
		t.Fatalf("merged settings = %s", data)
	}
}

func TestClaudeTUIBackendWithFakeClaude(t *testing.T) {
	if testing.Short() {
		t.Skip("PTY integration test")
	}
	tmp := t.TempDir()
	home := t.TempDir()
	cwd := t.TempDir()
	fake := tmp + "/fake-claude.py"
	script := `#!/usr/bin/env python3
import json, os, sys, time, tty
sid = ""
for i, a in enumerate(sys.argv):
    if a in ("--session-id", "--resume") and i + 1 < len(sys.argv): sid = sys.argv[i+1]
if not sid: sid = "fake-session"
path = os.path.join(os.environ["HOME"], ".claude", "projects", "fake", sid + ".jsonl")
os.makedirs(os.path.dirname(path), exist_ok=True)
os.write(1, b"\x1b[?2004h\xe2\x9d\xaf\xc2\xa0")
tty.setraw(0)
buf = b""
while not buf.endswith(b"\x1b[201~"):
    buf += os.read(0, 1)
rows = [
 {"type":"user", "message":{"content":"hello"}, "sessionId":sid, "cwd":os.getcwd(), "entrypoint":"cli"},
 {"type":"assistant", "message":{"id":"a1", "content":[{"type":"text","text":"fake reply"}], "stop_reason":"end_turn"}},
 {"type":"system", "subtype":"turn_duration", "terminal_reason":"end_turn"},
]
with open(path, "a", encoding="utf-8") as f:
    for row in rows:
        f.write(json.dumps(row) + "\n"); f.flush()
time.sleep(60)
`
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	backend := &claudeTUIBackend{cfg: Config{ClaudeMode: "tui", ExecutablePath: fake, Env: map[string]string{"HOME": home}}}
	started := time.Now()
	session, err := backend.Execute(t.Context(), "hello", ExecOptions{Cwd: cwd, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var result Result
	select {
	case result = <-session.Result:
	case <-time.After(10 * time.Second):
		t.Fatal("fake Claude did not finish")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("result cleanup took %s; expected direct process-group termination", elapsed)
	}
	if result.Status != "completed" || result.Output != "fake reply" {
		entries, _ := filepath.Glob(home + "/.claude/projects/fake/*")
		data, _ := os.ReadFile(entries[0])
		t.Fatalf("result = %+v files=%v home=%s data=%q", result, entries, home, data)
	}
	var status bool
	for msg := range session.Messages {
		if msg.Type == MessageStatus && msg.SessionID != "" {
			status = true
		}
	}
	if !status {
		t.Fatal("missing session status")
	}
	t.Log("fake transcript entrypoint: cli; session status captured")
}

func TestClaudeModeFactoryIsOptIn(t *testing.T) {
	stream, err := New("claude", Config{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := stream.(*claudeBackend); !ok {
		t.Fatalf("default Claude backend = %T", stream)
	}
	tui, err := New("claude", Config{ClaudeMode: "tui"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tui.(*claudeTUIBackend); !ok {
		t.Fatalf("tui Claude backend = %T", tui)
	}
}

func TestClaudeTUIPathWithin(t *testing.T) {
	if !pathWithin("/task/work/sub", "/task/work") || pathWithin("/task/worker", "/task/work") || pathWithin("/other", "/task/work") {
		t.Fatal("pathWithin ownership boundary incorrect")
	}
}

func TestClaudeTUIOwnedCwdProcessesFindsChildAfterLeaderExit(t *testing.T) {
	root := t.TempDir()
	cmd := exec.Command("/bin/sh", "-c", "sleep 60 & wait")
	cmd.Dir = root
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	claudeTUISignalOwned(cmd, ExecOptions{Cwd: root, CwdOwnedByDaemon: true}, syscall.SIGTERM)
	_ = cmd.Wait()
	if len(claudeTUIOwnedCwdProcesses(cmd, root)) != 0 {
		t.Fatal("owned descendant process survived group cancellation")
	}
}

func TestClaudeTUISignalOwnedKillsOnlyDescendants(t *testing.T) {
	root := t.TempDir()
	childFile := filepath.Join(root, "child.pid")
	cmd := exec.Command("/bin/sh", "-c", fmt.Sprintf("sleep 60 & echo $! > %q; wait", childFile))
	cmd.Dir = root
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var childPID int
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		if data, err := os.ReadFile(childFile); err == nil {
			_, _ = fmt.Sscan(string(data), &childPID)
			if childPID > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if childPID == 0 {
		t.Fatal("child pid was not written")
	}
	claudeTUISignalOwned(cmd, ExecOptions{Cwd: root, CwdOwnedByDaemon: false}, syscall.SIGTERM)
	time.Sleep(50 * time.Millisecond)
	if err := syscall.Kill(childPID, 0); err == nil {
		t.Fatalf("descendant process %d survived owned-tree cancellation", childPID)
	}
	_ = syscall.Kill(childPID, syscall.SIGKILL)
	_ = cmd.Wait()
}

func TestClaudeTUIScreenTextRemovesCursorMoves(t *testing.T) {
	raw := "\x1b[2GNo, exit\r\n\x1b[4GYes,\x1b[9GI\x1b[11Gtrust\x1b[17Gthis\x1b[22Gfolder"
	got := tuiScreenText(raw)
	if !strings.Contains(got, "No, exit") || !strings.Contains(got, "Yes,Itrustthisfolder") {
		t.Fatalf("screen text = %q", got)
	}
}

func TestClaudeTUIUnknownScreenTimesOutAndKillsProcess(t *testing.T) {
	previousTimeout := claudeTUIStartupTimeout
	claudeTUIStartupTimeout = time.Second
	defer func() { claudeTUIStartupTimeout = previousTimeout }()

	tmp := t.TempDir()
	home := t.TempDir()
	cwd := t.TempDir()
	fake := filepath.Join(tmp, "fake-unknown-claude.py")
	pidFile := filepath.Join(tmp, "pid")
	script := fmt.Sprintf(`#!/usr/bin/env python3
import os, time
with open(%q, "w") as f: f.write(str(os.getpid()))
os.write(1, b"\x1b[?2004h\x1b[?999zUNKNOWN-MODAL")
time.sleep(60)
`, pidFile)
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	backend := &claudeTUIBackend{cfg: Config{ClaudeMode: "tui", ExecutablePath: fake, Env: map[string]string{"HOME": home}}}
	session, err := backend.Execute(t.Context(), "hello", ExecOptions{Cwd: cwd, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-session.Result:
		if result.Status != "failed" || !strings.HasPrefix(result.Error, "claude tui startup timed out:") {
			t.Fatalf("result = %+v, want startup timeout", result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("unknown screen did not fail within startup timeout")
	}
	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	if _, err := fmt.Sscanf(string(pidBytes), "%d", &pid); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatalf("fake Claude pid %d survived startup timeout", pid)
	}
}

func TestClaudeTUITimeoutStatus(t *testing.T) {
	tmp, home, cwd := t.TempDir(), t.TempDir(), t.TempDir()
	fake := filepath.Join(tmp, "fake-timeout-claude.py")
	script := "#!/usr/bin/env python3\nimport time\ntime.sleep(60)\n"
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	backend := &claudeTUIBackend{cfg: Config{ClaudeMode: "tui", ExecutablePath: fake, Env: map[string]string{"HOME": home}}}
	session, err := backend.Execute(t.Context(), "hello", ExecOptions{Cwd: cwd, Timeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-session.Result:
		if result.Status != "timeout" {
			t.Fatalf("result = %+v, want timeout", result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout result not delivered")
	}
}
