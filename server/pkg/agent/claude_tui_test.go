package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClaudeTUIRejectedArg(t *testing.T) {
	for _, arg := range []string{"--output-format=json", "--prefill=hello", "--worktree", "-w=foo", "--include-hook-events"} {
		if !claudeTUIRejectedArg(arg) {
			t.Errorf("claudeTUIRejectedArg(%q) = false", arg)
		}
	}
	for _, arg := range []string{"--model=haiku", "--effort", "--no-chrome"} {
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
	backend.processTUIRow(row, ch, map[string]TokenUsage{}, map[string]struct{}{}, &finalText, &finalErr, &sawResult, &reason, &rows, "session-1", "")
	close(ch)
	for m := range ch {
		messages = append(messages, m)
	}
	if rows != 0 || sawResult || len(messages) != 0 {
		t.Fatalf("synthetic resume row changed state: rows=%d result=%v messages=%v", rows, sawResult, messages)
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
time.sleep(0.2)
`
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	backend := &claudeTUIBackend{cfg: Config{ClaudeMode: "tui", ExecutablePath: fake, Env: map[string]string{"HOME": home}}}
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

func TestClaudeTUIScreenTextRemovesCursorMoves(t *testing.T) {
	raw := "\x1b[2GNo, exit\r\n\x1b[4GYes,\x1b[9GI\x1b[11Gtrust\x1b[17Gthis\x1b[22Gfolder"
	got := tuiScreenText(raw)
	if !strings.Contains(got, "No, exit") || !strings.Contains(got, "Yes,Itrustthisfolder") {
		t.Fatalf("screen text = %q", got)
	}
}
