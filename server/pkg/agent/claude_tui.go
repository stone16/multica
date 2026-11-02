//go:build darwin || linux

package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"

	"github.com/google/uuid"
)

var claudeTUIStartupTimeout = 30 * time.Second

// claudeTUIBackend runs the published interactive Claude binary in a PTY. The
// transcript, rather than the rendered screen, owns semantic events.
type claudeTUIBackend struct{ cfg Config }

func (b *claudeTUIBackend) Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error) {
	if b.cfg.ClaudeMode != "tui" {
		return nil, errors.New("claude tui backend requires tui mode")
	}
	path := b.cfg.ExecutablePath
	if path == "" {
		path = "claude"
	}
	resolved, err := exec.LookPath(path)
	if err != nil {
		return nil, fmt.Errorf("claude executable not found at %q: %w", path, err)
	}
	if opts.MaxTurns > 0 {
		return nil, errors.New("claude tui does not support max turns")
	}
	for _, arg := range append(append([]string{}, opts.ExtraArgs...), opts.CustomArgs...) {
		if claudeTUIRejectedArg(arg) {
			return nil, fmt.Errorf("claude tui does not support custom argument %q", arg)
		}
	}
	runCtx, cancel := runContext(ctx, opts.Timeout)
	master, slave, err := openClaudeTUIpty()
	if err != nil {
		cancel()
		return nil, err
	}

	sessionID := opts.ResumeSessionID
	if sessionID == "" {
		sessionID = uuid.NewString()
	}
	args := []string{
		"--permission-mode", "bypassPermissions",
		"--disallowedTools", "AskUserQuestion,EnterPlanMode,ExitPlanMode,EnterWorktree,ExitWorktree",
		"--no-chrome",
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.ThinkingLevel != "" {
		args = append(args, "--effort", opts.ThinkingLevel)
	}
	if opts.ResumeSessionID != "" {
		args = append(args, "--resume", opts.ResumeSessionID)
	} else {
		args = append(args, "--session-id", sessionID)
	}
	settingsPath := opts.ClaudeSettingsPath
	generatedSettings := false
	if settingsPath == "" {
		settingsPath, err = writeClaudeTUISettings("")
		if err != nil {
			_ = master.Close()
			_ = slave.Close()
			cancel()
			return nil, err
		}
		generatedSettings = true
	}
	args = append(args, "--settings", settingsPath)
	var mcpPath string
	if hasManagedMcpConfig(opts.McpConfig) {
		mcpPath, err = writeMcpConfigToTemp(opts.McpConfig)
		if err != nil {
			_ = master.Close()
			_ = slave.Close()
			cancel()
			return nil, err
		}
		args = append(args, "--mcp-config", mcpPath, "--strict-mcp-config")
	}
	args = append(args, filterCustomArgs(opts.ExtraArgs, claudeBlockedArgs, b.cfg.Logger)...)
	args = append(args, filterCustomArgs(opts.CustomArgs, claudeBlockedArgs, b.cfg.Logger)...)

	cmd := b.cfg.commandAt(resolved).exec(runCtx, args...)
	b.cfg.logAgentCommand(cmd, newAgentCommandLogArgs(args))
	cmd.Dir = opts.Cwd
	cmd.Env = claudeTUIEnv(b.cfg.Env)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := claudeRootSudoPreflight(args, cmd.Env); err != nil {
		_ = master.Close()
		_ = slave.Close()
		cancel()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = master.Close()
		_ = slave.Close()
		cancel()
		if mcpPath != "" {
			cleanupMcpConfigTemp(mcpPath)
		}
		return nil, fmt.Errorf("start claude tui: %w", err)
	}
	_ = slave.Close()
	watchdogR, watchdogW, err := os.Pipe()
	if err != nil {
		claudeTUISignal(cmd, syscall.SIGKILL)
		_ = master.Close()
		cancel()
		return nil, fmt.Errorf("create claude tui watchdog pipe: %w", err)
	}
	watchdog := exec.Command("/bin/sh", "-c", "read state || state=abort; [ \"$state\" = abort ] || exit 0; kill -TERM -$1 2>/dev/null || exit 0; i=0; while kill -0 -$1 2>/dev/null; do i=$((i+1)); [ \"$i\" -ge 100 ] && break; sleep 0.1; done; kill -KILL -$1 2>/dev/null || true", "claude-tui-watchdog", strconv.Itoa(cmd.Process.Pid))
	watchdog.Stdin = watchdogR
	watchdog.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := watchdog.Start(); err != nil {
		_ = watchdogR.Close()
		_ = watchdogW.Close()
		claudeTUISignal(cmd, syscall.SIGKILL)
		_ = master.Close()
		cancel()
		return nil, fmt.Errorf("start claude tui watchdog: %w", err)
	}
	_ = watchdogR.Close()

	messages := make(chan Message, 256)
	results := make(chan Result, 1)
	procDone := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(procDone)
	}()
	var terminal atomic.Bool
	go b.consumeTUI(runCtx, cancel, cmd, master, procDone, watchdogW, watchdog, prompt, sessionID, opts, messages, results, &terminal, mcpPath, generatedSettings, settingsPath)

	return &Session{
		CleanupRequired:  true,
		TerminalObserved: terminal.Load,
		Messages:         messages,
		Result:           results,
	}, nil
}

func claudeTUIRejectedArg(raw string) bool {
	flag := unshellQuoteArg(raw)
	if i := strings.IndexByte(flag, '='); i >= 0 {
		flag = flag[:i]
	}
	switch flag {
	case "--max-turns", "--max-budget-usd", "--json-schema", "--input-format", "--output-format", "--include-partial-messages", "--include-hook-events", "--replay-user-messages", "--no-session-persistence", "--fork-session", "--worktree", "-w", "--tmux", "--init-only", "--from-pr", "--prefill", "--prefill-b64", "--settings", "--disallowedTools", "--session-id", "--resume", "--no-chrome", "--chrome":
		return true
	default:
		return false
	}
}

func writeClaudeTUISettings(existing string) (string, error) {
	settings := map[string]any{"skipDangerousModePermissionPrompt": true, "permissions": map[string]any{"deny": []string{"AskUserQuestion", "EnterWorktree", "ExitWorktree"}}}
	if existing != "" {
		data, err := os.ReadFile(existing)
		if err != nil {
			return "", fmt.Errorf("read Claude TUI settings: %w", err)
		}
		if err := json.Unmarshal(data, &settings); err != nil {
			return "", fmt.Errorf("parse Claude TUI settings: %w", err)
		}
	}
	settings["skipDangerousModePermissionPrompt"] = true
	settings["permissions"] = map[string]any{"deny": []string{"AskUserQuestion", "EnterWorktree", "ExitWorktree"}}
	data, err := json.Marshal(settings)
	if err != nil {
		return "", fmt.Errorf("marshal Claude TUI settings: %w", err)
	}
	f, err := os.CreateTemp("", "multica-claude-tui-settings-*.json")
	if err != nil {
		return "", fmt.Errorf("create Claude TUI settings: %w", err)
	}
	name := f.Name()
	if err := f.Chmod(0o600); err == nil {
		_, err = f.Write(data)
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		_ = os.Remove(name)
		if err == nil {
			err = closeErr
		}
		return "", fmt.Errorf("write Claude TUI settings: %w", err)
	}
	return name, nil
}

func claudeTUIEnv(extra map[string]string) []string {
	env := mergeEnv(os.Environ(), extra)
	out := env[:0]
	for _, item := range env {
		key, _, _ := strings.Cut(item, "=")
		if isFilteredChildEnvKey(key) || key == "CLAUDE_CODE_CHILD_SESSION" || key == "CLAUDE_CODE_SKIP_PROMPT_HISTORY" || key == "CLAUDE_CODE_SESSION_ATTENDED" || strings.HasPrefix(key, "CLAUDE_CODE_MESSAGING_") {
			continue
		}
		out = append(out, item)
	}
	out = append(out, "CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1", "DISABLE_AUTOUPDATER=1", "MULTICA_CLAUDE_TUI=1")
	return out
}

func (b *claudeTUIBackend) consumeTUI(ctx context.Context, cancel context.CancelFunc, cmd *exec.Cmd, master *os.File, procDone <-chan struct{}, watchdogW *os.File, watchdog *exec.Cmd, prompt, sessionID string, opts ExecOptions, messages chan Message, results chan Result, terminal *atomic.Bool, mcpPath string, generatedSettings bool, settingsPath string) {
	defer cancel()
	defer close(messages)
	defer close(results)
	defer func() {
		_ = watchdogW.Close()
		_ = watchdog.Wait()
	}()
	if mcpPath != "" {
		defer cleanupMcpConfigTemp(mcpPath)
	}
	if generatedSettings {
		defer os.Remove(settingsPath)
	}

	cfgDir, _ := claudeConfigDir(cmd.Env, opts.Cwd)
	var mu sync.Mutex
	var screen strings.Builder
	var bracketedPasteSeen bool
	var pasted, entered bool
	var trustAccepted bool
	trustMoves := 0
	var trustSince time.Time
	var enterAt time.Time
	var pasteAt time.Time
	var readySince time.Time
	enterAttempts := 0
	var promptRows int
	var finalText string
	var finalErr string
	var sawResult bool
	var terminalReason string
	usage := map[string]TokenUsage{}
	usageBaseline := map[string]TokenUsage{}
	seenMessage := map[string]struct{}{}
	pathOffsets := map[string]int64{}
	if opts.ResumeSessionID != "" {
		if p, err := findClaudeSessionFile(cfgDir, opts.ResumeSessionID); err == nil {
			if st, statErr := os.Stat(p); statErr == nil {
				pathOffsets[p] = st.Size()
				if f, openErr := os.Open(p); openErr == nil {
					usageBaseline, _, _ = readLastClaudeCostStateUsage(f, 0, st.Size())
					_ = f.Close()
				}
			}
		}
	}

	readDone := make(chan struct{})
	ptyBytes := make(chan []byte, 32)
	go func() {
		defer close(readDone)
		buf := make([]byte, 8192)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				chunk := append([]byte(nil), buf[:n]...)
				select {
				case ptyBytes <- chunk:
				default:
				}
			}
			if err != nil {
				return
			}
		}
	}()

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	startup := time.NewTimer(claudeTUIStartupTimeout)
	defer startup.Stop()
	var waitStart time.Time
	for {
		select {
		case <-ctx.Done():
			_, _ = watchdogW.WriteString("abort\n")
			_ = watchdogW.Close()
			claudeTUISignalOwned(cmd, opts, syscall.SIGTERM)
			select {
			case <-procDone:
			case <-time.After(10 * time.Second):
				claudeTUISignalOwned(cmd, opts, syscall.SIGKILL)
			}
			_ = master.Close()
			<-readDone
			_ = b.readTUITranscript(cfgDir, sessionID, pathOffsets, messages, usage, usageBaseline, seenMessage, &finalText, &finalErr, &sawResult, &terminalReason, &promptRows, opts.Cwd)
			results <- Result{Status: "cancelled", Error: "execution cancelled", SessionID: sessionID, Usage: usage}
			return
		case chunk, ok := <-ptyBytes:
			if !ok {
				ptyBytes = nil
				continue
			}
			respondTUIQueries(master, chunk)
			mu.Lock()
			screen.Write(chunk)
			if bytes.Contains(chunk, []byte("\x1b[?2004h")) {
				bracketedPasteSeen = true
			}
			if screen.Len() > 128<<10 {
				s := screen.String()
				screen.Reset()
				screen.WriteString(s[len(s)-64<<10:])
			}
			rawScreen := screen.String()
			s := tuiScreenText(rawScreen)
			compact := strings.Map(func(r rune) rune {
				if unicode.IsSpace(r) {
					return -1
				}
				return r
			}, s)
			trust := !trustAccepted && strings.Contains(compact, "No,exit") && strings.Contains(compact, "Yes,Itrustthisfolder")
			if trust && trustSince.IsZero() {
				trustSince = time.Now()
			}
			if !trust {
				trustSince = time.Time{}
			}
			if tuiComposerReady(s) && bracketedPasteSeen {
				if readySince.IsZero() {
					readySince = time.Now()
				}
			} else {
				readySince = time.Time{}
			}
			mu.Unlock()
		case <-ticker.C:
			mu.Lock()
			compact := strings.Map(func(r rune) rune {
				if unicode.IsSpace(r) {
					return -1
				}
				return r
			}, tuiScreenText(screen.String()))
			if !trustAccepted && !trustSince.IsZero() && time.Since(trustSince) > 300*time.Millisecond {
				if strings.Contains(compact, "❯No,exit") && trustMoves == 0 {
					_, _ = master.Write([]byte("\x1b[B"))
					trustMoves = 1
					trustSince = time.Now()
				} else if strings.Contains(compact, "❯Yes,Itrustthisfolder") && trustMoves == 1 {
					_, _ = master.Write([]byte("\r"))
					trustAccepted = true
					trustSince = time.Time{}
					trustMoves = 0
					screen.Reset()
				}
			}
			if !pasted && !readySince.IsZero() && time.Since(readySince) > 300*time.Millisecond {
				clean := tuiPasteText(prompt)
				_, _ = master.Write([]byte("\x1b[200~" + clean + "\x1b[201~"))
				pasted = true
				pasteAt = time.Now()
				waitStart = pasteAt
			}
			if pasted && !entered && time.Since(waitStart) > 150*time.Millisecond {
				_, _ = master.Write([]byte("\r"))
				entered = true
				enterAttempts = 1
				enterAt = time.Now()
			}
			mu.Unlock()
			if err := b.readTUITranscript(cfgDir, sessionID, pathOffsets, messages, usage, usageBaseline, seenMessage, &finalText, &finalErr, &sawResult, &terminalReason, &promptRows, opts.Cwd); err != nil && finalErr == "" {
				finalErr = err.Error()
			}
			if entered && promptRows == 0 && enterAttempts < 3 && time.Since(enterAt) > 3*time.Second && time.Since(enterAt) < 10*time.Second {
				mu.Lock()
				_, _ = master.Write([]byte("\r"))
				mu.Unlock()
				enterAttempts++
			}
			if pasted && promptRows == 0 && time.Since(pasteAt) > 10*time.Second {
				claudeTUISignalOwned(cmd, opts, syscall.SIGTERM)
				results <- Result{Status: "failed", Error: "claude tui produced no transcript receipt within 10s", SessionID: sessionID, Usage: usage}
				return
			}
			if sawResult && !terminal.Load() {
				terminal.Store(true)
				mu.Lock()
				_, _ = master.Write([]byte{3})
				mu.Unlock()
				// Claude can leave the interactive shell alive after writing its
				// terminal transcript row. Signal the owned process group directly
				// so Result also acknowledges bounded cleanup.
				claudeTUISignalOwned(cmd, opts, syscall.SIGTERM)
			}
		case <-startup.C:
			_, _ = watchdogW.WriteString("abort\n")
			_ = watchdogW.Close()
			claudeTUISignalOwned(cmd, opts, syscall.SIGTERM)
			select {
			case <-procDone:
			case <-time.After(10 * time.Second):
				claudeTUISignalOwned(cmd, opts, syscall.SIGKILL)
				<-procDone
			}
			_ = master.Close()
			<-readDone
			mu.Lock()
			tail := tuiScreenTail(screen.String())
			mu.Unlock()
			results <- Result{Status: "failed", Error: "claude tui startup timed out: " + tail, SessionID: sessionID, Usage: usage}
			return
		case <-procDone:
			_, _ = watchdogW.WriteString("done\n")
			_ = watchdogW.Close()
			// The process can exit before the 50ms poll; drain one final tail so
			// a fast fake (and short real turn) does not lose its terminal row.
			_ = b.readTUITranscript(cfgDir, sessionID, pathOffsets, messages, usage, usageBaseline, seenMessage, &finalText, &finalErr, &sawResult, &terminalReason, &promptRows, opts.Cwd)
			_ = master.Close()
			<-readDone
			if !sawResult {
				finalErr = "claude tui exited without a terminal transcript result"
			}
			status := "completed"
			if finalErr != "" {
				status = "failed"
			}
			results <- Result{Status: status, Output: finalText, Error: finalErr, SessionID: sessionID, Usage: usage}
			return
		}
	}
}

// respondTUIQueries answers the device-status queries Claude uses to finish
// terminal setup. A PTY without these replies can exit immediately after the
// trust screen even though the process and login are otherwise healthy.
func respondTUIQueries(master *os.File, chunk []byte) {
	if bytes.Contains(chunk, []byte("\x1b[c")) || bytes.Contains(chunk, []byte("\x1b[0c")) {
		_, _ = master.Write([]byte("\x1b[?62;c"))
	}
	if bytes.Contains(chunk, []byte("\x1b[6n")) {
		_, _ = master.Write([]byte("\x1b[1;1R"))
	}
}

func tuiComposerReady(screen string) bool {
	if !strings.Contains(screen, "❯\u00a0") {
		return false
	}
	compact := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, screen)
	for _, blocked := range []string{"❯1.", "❯2.", "Entertoconfirm", "Esctocancel", "No,exit", "Yes,Itrustthisfolder"} {
		if strings.Contains(compact, blocked) {
			return false
		}
	}
	return true
}

func tuiScreenTail(raw string) string {
	clean := tuiScreenText(raw)
	if len(clean) > 512 {
		clean = clean[len(clean)-512:]
	}
	return strings.TrimSpace(clean)
}

// tuiScreenText removes cursor movement and OSC/CSI control sequences while
// retaining the text that Claude renders between them for modal matching.
func tuiScreenText(raw string) string {
	var b strings.Builder
	for i := 0; i < len(raw); i++ {
		if raw[i] != 0x1b {
			if raw[i] >= 0x20 || raw[i] == '\n' || raw[i] == '\r' || raw[i] == '\t' {
				b.WriteByte(raw[i])
			}
			continue
		}
		i++
		if i >= len(raw) {
			break
		}
		if raw[i] == ']' {
			for i+1 < len(raw) {
				i++
				if raw[i] == 0x07 || (raw[i] == 0x1b && i+1 < len(raw) && raw[i+1] == '\\') {
					if raw[i] == 0x1b {
						i++
					}
					break
				}
			}
			continue
		}
		if raw[i] == '[' {
			for i+1 < len(raw) {
				i++
				if raw[i] >= 0x40 && raw[i] <= 0x7e {
					break
				}
			}
		}
	}
	return b.String()
}

func tuiPasteText(prompt string) string {
	var b strings.Builder
	for _, r := range prompt {
		if r < 0x20 && r != '\n' && r != '\t' || r == 0x7f || r == 0x1b {
			continue
		}
		b.WriteRune(r)
	}
	s := b.String()
	trim := strings.TrimLeft(s, " \t\n")
	if len(trim) > 0 && strings.ContainsRune("/!#&", []rune(trim)[0]) {
		s = "\n" + s
	}
	return s
}

func claudeTUISignal(cmd *exec.Cmd, sig syscall.Signal) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, sig); err != nil {
		_ = cmd.Process.Signal(sig)
	}
}

// claudeTUISignalOwned keeps the cwd census behind an explicit ownership bit.
// A local-directory task has no proof that a process in the user's directory
// belongs to this run, so it receives only the owned process-group signal.
func claudeTUISignalOwned(cmd *exec.Cmd, opts ExecOptions, sig syscall.Signal) {
	claudeTUISignal(cmd, sig)
	if !opts.CwdOwnedByDaemon || opts.Cwd == "" || (runtime.GOOS != "darwin" && runtime.GOOS != "linux") {
		return
	}
	for _, p := range claudeTUIOwnedCwdProcesses(cmd, opts.Cwd) {
		if p == cmd.Process.Pid {
			continue
		}
		_ = syscall.Kill(-p, sig)
	}
}

// claudeTUIOwnedCwdProcesses finds processes in the daemon-owned task cwd
// that remain in Claude's session. A child that creates a new session is
// intentionally skipped because ownership is no longer provable.
func claudeTUIOwnedCwdProcesses(cmd *exec.Cmd, root string) []int {
	sessionColumn := "sid="
	if runtime.GOOS == "darwin" {
		sessionColumn = "sess="
	}
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,pgid=,"+sessionColumn).Output()
	if err != nil {
		return nil
	}
	ownSID := strconv.Itoa(cmd.Process.Pid)
	type proc struct{ pid, ppid, pgid, sid int }
	var rows []proc
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 4 {
			continue
		}
		pid, e1 := strconv.Atoi(fields[0])
		ppid, e2 := strconv.Atoi(fields[1])
		pgid, e3 := strconv.Atoi(fields[2])
		sid, e4 := strconv.Atoi(fields[3])
		if e1 != nil || e2 != nil || e3 != nil || e4 != nil {
			continue
		}
		rows = append(rows, proc{pid: pid, ppid: ppid, pgid: pgid, sid: sid})
	}
	var pids []int
	for _, row := range rows {
		if strconv.Itoa(row.sid) != ownSID || row.pid == os.Getpid() || row.pid == cmd.Process.Pid || row.pgid <= 1 {
			continue
		}
		cwd, ok := claudeTUIProcessCwd(row.pid)
		if ok && pathWithin(cwd, root) {
			pids = append(pids, row.pgid)
		}
	}
	return pids
}

func claudeTUIProcessCwd(pid int) (string, bool) {
	if runtime.GOOS == "linux" {
		cwd, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "cwd"))
		return cwd, err == nil
	}
	out, err := exec.Command("lsof", "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn").Output()
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "n") {
			return strings.TrimPrefix(line, "n"), true
		}
	}
	return "", false
}

func pathWithin(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func claudeTUISamePath(a, b string) bool {
	a, _ = filepath.EvalSymlinks(a)
	b, _ = filepath.EvalSymlinks(b)
	return filepath.Clean(a) == filepath.Clean(b)
}

func (b *claudeTUIBackend) readTUITranscript(cfgDir, sessionID string, offsets map[string]int64, ch chan<- Message, usage, baseline map[string]TokenUsage, seen map[string]struct{}, finalText *string, finalErr *string, sawResult *bool, terminalReason *string, promptRows *int, expectedCwd string) error {
	paths, _ := filepath.Glob(filepath.Join(cfgDir, "projects", "*", sessionID+".jsonl"))
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		off := offsets[path]
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			_ = f.Close()
			continue
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64<<10), agentStreamMaxLineBytes)
		for scanner.Scan() {
			line := scanner.Bytes()
			var raw map[string]any
			if json.Unmarshal(line, &raw) != nil {
				continue
			}
			b.processTUIRow(raw, ch, usage, baseline, seen, finalText, finalErr, sawResult, terminalReason, promptRows, sessionID, expectedCwd)
			off += int64(len(line) + 1)
		}
		offInfo, _ := f.Seek(0, io.SeekCurrent)
		if offInfo > off {
			off = offInfo
		}
		offsets[path] = off
		_ = f.Close()
	}
	return nil
}

func (b *claudeTUIBackend) processTUIRow(raw map[string]any, ch chan<- Message, usage, baseline map[string]TokenUsage, seen map[string]struct{}, finalText, finalErr *string, sawResult *bool, terminalReason *string, promptRows *int, sessionID, expectedCwd string) {
	typ, _ := raw["type"].(string)
	switch typ {
	case "user":
		msg, _ := raw["message"].(map[string]any)
		content := msg["content"]
		if isMeta, _ := raw["isMeta"].(bool); isMeta {
			return
		}
		blocks, _ := content.([]any)
		receipt := false
		if text, ok := content.(string); ok && strings.TrimSpace(text) != "" {
			receipt = true
		}
		for _, x := range blocks {
			bmap, _ := x.(map[string]any)
			if bmap["type"] == "tool_result" {
				// A synthetic resume recovery result before the human prompt is
				// intentionally ignored; normal tool results remain observable.
				if *promptRows > 0 {
					id, _ := bmap["tool_use_id"].(string)
					trySend(ch, Message{Type: MessageToolResult, CallID: id, Output: fmt.Sprint(bmap["content"])})
				}
				continue
			}
			if text, ok := x.(string); ok && strings.TrimSpace(text) != "" {
				receipt = true
			}
			if bmap["type"] == "text" {
				if text, _ := bmap["text"].(string); strings.TrimSpace(text) != "" {
					receipt = true
				}
			}
		}
		if receipt && *promptRows == 0 {
			*promptRows = 1
			if got, ok := raw["sessionId"].(string); ok && got != "" && got != sessionID {
				*finalErr = fmt.Sprintf("Claude transcript session id %q differs from requested %q", got, sessionID)
			}
			if got, ok := raw["entrypoint"].(string); ok && got != "cli" {
				*finalErr = fmt.Sprintf("Claude transcript entrypoint %q is not cli", got)
			}
			if got, ok := raw["cwd"].(string); ok && expectedCwd != "" && !claudeTUISamePath(got, expectedCwd) {
				*finalErr = fmt.Sprintf("Claude transcript cwd %q differs from task cwd %q", got, expectedCwd)
			}
			if *finalErr == "" {
				trySend(ch, Message{Type: MessageStatus, Status: "running", SessionID: sessionID})
			}
		}
	case "assistant":
		msg, _ := raw["message"].(map[string]any)
		if *promptRows == 0 {
			// Synthetic recovery assistant rows (for example, stop_sequence
			// after an interrupted tool) are not part of the new turn.
			if !isAPIErrorRow(raw) {
				return
			}
		}
		id, _ := msg["id"].(string)
		if id != "" {
			if _, duplicate := seen[id]; duplicate {
				return
			}
			seen[id] = struct{}{}
		}
		if u, ok := msg["usage"].(map[string]any); ok {
			model, _ := msg["model"].(string)
			if model != "" {
				usage[model] = TokenUsage{InputTokens: int64(number(u["input_tokens"])), OutputTokens: int64(number(u["output_tokens"])), CacheReadTokens: int64(number(u["cache_read_input_tokens"])), CacheWriteTokens: int64(number(u["cache_creation_input_tokens"]))}
			}
		}
		blocks, _ := msg["content"].([]any)
		for _, x := range blocks {
			bmap, _ := x.(map[string]any)
			typ, _ := bmap["type"].(string)
			switch typ {
			case "text":
				t, _ := bmap["text"].(string)
				if *finalText != "" && t != "" {
					*finalText += "\n"
				}
				*finalText += t
				trySend(ch, Message{Type: MessageText, Content: t})
			case "thinking":
				t, _ := bmap["thinking"].(string)
				trySend(ch, Message{Type: MessageThinking, Content: t})
			case "tool_use":
				call, _ := bmap["id"].(string)
				name, _ := bmap["name"].(string)
				in, _ := bmap["input"].(map[string]any)
				trySend(ch, Message{Type: MessageToolUse, CallID: call, Tool: name, Input: in})
			}
		}
		if isAPIErrorRow(raw) {
			*finalErr = fmt.Sprint(raw["error"])
			if *finalErr == "<nil>" || *finalErr == "" {
				*finalErr = fmt.Sprint(raw["message"])
			}
		}
	case "system":
		sub, _ := raw["subtype"].(string)
		if sub == "turn_duration" && *promptRows > 0 {
			*sawResult = true
			if r, ok := raw["terminal_reason"].(string); ok {
				*terminalReason = r
				if errText := claudeTerminalReasonFailure(r, ""); errText != "" {
					*finalErr = errText
				}
			}
		}
	case "cost-state":
		models, _ := raw["modelUsage"].(map[string]any)
		for model, val := range models {
			m, _ := val.(map[string]any)
			current := TokenUsage{InputTokens: int64(number(m["inputTokens"])), OutputTokens: int64(number(m["outputTokens"])), CacheReadTokens: int64(number(m["cacheReadInputTokens"])), CacheWriteTokens: int64(number(m["cacheCreationInputTokens"]))}
			if base, ok := baseline[model]; ok && current.InputTokens >= base.InputTokens && current.OutputTokens >= base.OutputTokens && current.CacheReadTokens >= base.CacheReadTokens && current.CacheWriteTokens >= base.CacheWriteTokens {
				current.InputTokens -= base.InputTokens
				current.OutputTokens -= base.OutputTokens
				current.CacheReadTokens -= base.CacheReadTokens
				current.CacheWriteTokens -= base.CacheWriteTokens
			}
			usage[model] = current
		}
	}
}

func isAPIErrorRow(raw map[string]any) bool {
	if v, _ := raw["isApiErrorMessage"].(bool); v {
		return true
	}
	_, hasError := raw["error"]
	_, hasDetails := raw["errorDetails"]
	return hasError || hasDetails
}

func number(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	}
	return 0
}
