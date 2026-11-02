//go:build darwin || linux

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
	settingsPath, err := writeClaudeTUISettings(opts.ClaudeSettingsPath)
	if err != nil {
		_ = master.Close()
		_ = slave.Close()
		cancel()
		return nil, err
	}
	generatedSettings := true
	args = append(args, "--settings", settingsPath)
	var mcpPath string
	if hasManagedMcpConfig(opts.McpConfig) {
		mcpPath, err = writeMcpConfigToTemp(opts.McpConfig)
		if err != nil {
			_ = os.Remove(settingsPath)
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
	// Let the backend perform graceful TERM -> bounded KILL cleanup. The
	// default CommandContext cancellation kills only the leader and leaks tools.
	cmd.Cancel = func() error { return nil }
	cmd.WaitDelay = 10 * time.Second
	if err := claudeRootSudoPreflight(args, cmd.Env); err != nil {
		_ = os.Remove(settingsPath)
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
		_ = os.Remove(settingsPath)
		return nil, fmt.Errorf("start claude tui: %w", err)
	}
	rootInfo, err := readCursorUnixProcessInfo(cmd.Process.Pid)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = master.Close()
		_ = os.Remove(settingsPath)
		cancel()
		return nil, fmt.Errorf("inspect claude tui process: %w", err)
	}
	_ = slave.Close()
	watchdogR, watchdogW, err := os.Pipe()
	if err != nil {
		claudeTUISignal(cmd, syscall.SIGKILL)
		_ = master.Close()
		_ = os.Remove(settingsPath)
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
		_ = os.Remove(settingsPath)
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
	go b.consumeTUI(runCtx, cancel, cmd, master, procDone, watchdogW, watchdog, prompt, sessionID, rootInfo.start, opts, messages, results, &terminal, mcpPath, generatedSettings, settingsPath)

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
	settings := map[string]any{}
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
	permissions, _ := settings["permissions"].(map[string]any)
	if permissions == nil {
		permissions = map[string]any{}
	}
	deny := make([]string, 0, 3)
	switch values := permissions["deny"].(type) {
	case []any:
		for _, value := range values {
			if item, ok := value.(string); ok {
				deny = append(deny, item)
			}
		}
	case []string:
		deny = append(deny, values...)
	}
	for _, item := range []string{"AskUserQuestion", "EnterWorktree", "ExitWorktree"} {
		found := false
		for _, current := range deny {
			if current == item {
				found = true
				break
			}
		}
		if !found {
			deny = append(deny, item)
		}
	}
	permissions["deny"] = deny
	settings["permissions"] = permissions
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

func (b *claudeTUIBackend) consumeTUI(ctx context.Context, cancel context.CancelFunc, cmd *exec.Cmd, master *os.File, procDone <-chan struct{}, watchdogW *os.File, watchdog *exec.Cmd, prompt, sessionID string, rootStart uint64, opts ExecOptions, messages chan Message, results chan Result, terminal *atomic.Bool, mcpPath string, generatedSettings bool, settingsPath string) {
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
	var trustModalSeen bool
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
	var cleanupStarted bool
	var cleanupKilled bool
	var cleanupAt time.Time
	var cleanupGroups []int
	var cleanupIdentities map[int]map[int]uint64
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
	ptyBytes := make(chan []byte, 256)
	go func() {
		defer close(readDone)
		buf := make([]byte, 8192)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				chunk := append([]byte(nil), buf[:n]...)
				ptyBytes <- chunk
			}
			if err != nil {
				if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
					time.Sleep(10 * time.Millisecond)
					continue
				}
				return
			}
		}
	}()

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	startup := time.NewTimer(claudeTUIStartupTimeout)
	defer startup.Stop()
	startupWaiting := true
	var waitStart time.Time
	for {
		select {
		case <-ctx.Done():
			_, _ = watchdogW.WriteString("abort\n")
			_ = watchdogW.Close()
			claudeTUIStop(cmd, opts, procDone, rootStart)
			_ = master.Close()
			waitClaudeTUIRead(readDone)
			_ = b.readTUITranscript(cfgDir, sessionID, pathOffsets, messages, usage, usageBaseline, seenMessage, &finalText, &finalErr, &sawResult, &terminalReason, &promptRows, opts.Cwd)
			if terminal.Load() {
				status := "completed"
				if finalErr != "" {
					status = "failed"
				}
				resultSession, rejected := tuiResultSession(opts, sessionID, status == "failed", promptRows, finalErr)
				results <- Result{Status: status, Output: finalText, Error: finalErr, SessionID: resultSession, ResumeRejected: rejected, Usage: usage}
				return
			}
			emittedSession := ""
			if promptRows > 0 {
				emittedSession = sessionID
			}
			resultSession, rejected := tuiResultSession(opts, emittedSession, false, promptRows, finalErr)
			status, errText := "cancelled", "execution cancelled"
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				status, errText = "timeout", fmt.Sprintf("execution timed out after %s", opts.Timeout)
			}
			results <- Result{Status: status, Error: errText, SessionID: resultSession, ResumeRejected: rejected, Usage: usage}
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
				trustModalSeen = true
			}
			if !trust && !trustModalSeen {
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
			if !trustAccepted && trustModalSeen && !trustSince.IsZero() && time.Since(trustSince) > 300*time.Millisecond {
				if trustMoves == 0 {
					_, _ = master.Write([]byte("\x1b[B"))
					trustMoves = 1
					trustSince = time.Now()
				} else if trustMoves == 1 {
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
			if err := b.readTUITranscript(cfgDir, sessionID, pathOffsets, messages, usage, usageBaseline, seenMessage, &finalText, &finalErr, &sawResult, &terminalReason, &promptRows, opts.Cwd); err != nil {
				finalErr = err.Error()
				_, _ = watchdogW.WriteString("abort\n")
				claudeTUIStop(cmd, opts, procDone, rootStart)
				_ = master.Close()
				waitClaudeTUIRead(readDone)
				resultSession, rejected := tuiResultSession(opts, "", true, promptRows, finalErr)
				results <- Result{Status: "failed", Error: finalErr, SessionID: resultSession, ResumeRejected: rejected, Usage: usage}
				return
			}
			if entered && promptRows == 0 && enterAttempts < 3 && time.Since(enterAt) > 3*time.Second && time.Since(enterAt) < 10*time.Second {
				mu.Lock()
				_, _ = master.Write([]byte("\r"))
				mu.Unlock()
				enterAttempts++
				enterAt = time.Now()
			}
			if pasted && promptRows == 0 && time.Since(pasteAt) > 10*time.Second {
				_, _ = watchdogW.WriteString("abort\n")
				claudeTUIStop(cmd, opts, procDone, rootStart)
				_ = master.Close()
				waitClaudeTUIRead(readDone)
				resultSession, rejected := tuiResultSession(opts, sessionID, true, promptRows, "claude tui produced no transcript receipt within 10s")
				results <- Result{Status: "failed", Error: "claude tui produced no transcript receipt within 10s", SessionID: resultSession, ResumeRejected: rejected, Usage: usage}
				return
			}
			if startupWaiting && promptRows > 0 {
				startupWaiting = false
				if !startup.Stop() {
					select {
					case <-startup.C:
					default:
					}
				}
			}
			if promptRows > 0 && tuiHumanPrompt(compact) {
				errText := "claude tui is waiting for interactive confirmation"
				_, _ = watchdogW.WriteString("abort\n")
				claudeTUIStop(cmd, opts, procDone, rootStart)
				_ = master.Close()
				waitClaudeTUIRead(readDone)
				resultSession, rejected := tuiResultSession(opts, sessionID, true, promptRows, errText)
				results <- Result{Status: "failed", Error: errText, SessionID: resultSession, ResumeRejected: rejected, Usage: usage}
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
				if rootStart == 0 || claudeTUIRootMatches(cmd, rootStart) {
					cleanupGroups, cleanupIdentities = claudeTUIOwnedProcessGroups(cmd)
					claudeTUISignalCaptured(cmd, cleanupGroups, cleanupIdentities, syscall.SIGTERM, rootStart)
				}
				cleanupStarted = true
				cleanupAt = time.Now()
			}
			if cleanupStarted && !cleanupKilled && time.Since(cleanupAt) >= 10*time.Second {
				claudeTUISignalCaptured(cmd, cleanupGroups, cleanupIdentities, syscall.SIGKILL, rootStart)
				cleanupKilled = true
			}
		case <-startup.C:
			if !startupWaiting {
				continue
			}
			_, _ = watchdogW.WriteString("abort\n")
			_ = watchdogW.Close()
			claudeTUIStop(cmd, opts, procDone, rootStart)
			_ = master.Close()
			waitClaudeTUIRead(readDone)
			mu.Lock()
			tail := tuiScreenTail(screen.String())
			mu.Unlock()
			errText := "claude tui startup timed out: " + tail
			resultSession, rejected := tuiResultSession(opts, sessionID, true, promptRows, errText)
			results <- Result{Status: "failed", Error: errText, SessionID: resultSession, ResumeRejected: rejected, Usage: usage}
			return
		case <-procDone:
			_, _ = watchdogW.WriteString("done\n")
			_ = watchdogW.Close()
			// The process can exit before the 50ms poll; drain one final tail so
			// a fast fake (and short real turn) does not lose its terminal row.
			if err := b.readTUITranscript(cfgDir, sessionID, pathOffsets, messages, usage, usageBaseline, seenMessage, &finalText, &finalErr, &sawResult, &terminalReason, &promptRows, opts.Cwd); err != nil && finalErr == "" {
				finalErr = err.Error()
			}
			_ = master.Close()
			waitClaudeTUIRead(readDone)
			if !sawResult && finalErr == "" {
				mu.Lock()
				finalErr = "claude tui exited without a terminal transcript result: " + tuiScreenTail(screen.String())
				mu.Unlock()
			}
			status := "completed"
			if finalErr != "" {
				status = "failed"
			}
			resultSession, rejected := tuiResultSession(opts, sessionID, status == "failed", promptRows, finalErr)
			results <- Result{Status: status, Output: finalText, Error: finalErr, SessionID: resultSession, ResumeRejected: rejected, Usage: usage}
			return
		}
	}
}

// tuiResultSession never returns the requested resume id when the run failed
// before Claude accepted a new prompt. Keeping that id would make the daemon
// retry a known stale transcript instead of clearing it.
func tuiResultSession(opts ExecOptions, emitted string, failed bool, promptRows int, errText string) (string, bool) {
	rejected := resumeWasRejected(opts.ResumeSessionID, emitted, failed, errText)
	if !rejected && failed && opts.ResumeSessionID != "" {
		compact := strings.Map(func(r rune) rune {
			if unicode.IsSpace(r) {
				return -1
			}
			return unicode.ToLower(r)
		}, errText)
		rejected = strings.Contains(compact, "noconversationfoundwithsessionid")
	}
	if rejected || promptRows == 0 {
		return "", rejected
	}
	return emitted, rejected
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

func tuiHumanPrompt(screen string) bool {
	compact := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, screen)
	return strings.Contains(compact, "Doyouwanttoproceed?") || strings.Contains(compact, "Entertoconfirm") && strings.Contains(compact, "Esc")
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

func claudeTUISignalOwned(cmd *exec.Cmd, opts ExecOptions, sig syscall.Signal) {
	claudeTUISignalOwnedIdentity(cmd, opts, sig, 0)
}

func claudeTUISignalOwnedIdentity(cmd *exec.Cmd, opts ExecOptions, sig syscall.Signal, expectedStart uint64) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if expectedStart != 0 {
		info, err := readCursorUnixProcessInfo(cmd.Process.Pid)
		if err != nil || info.start != expectedStart {
			return
		}
	}
	// Capture descendants before signalling Claude; the leader may exit
	// immediately and lose the parent relationship. The census follows the
	// verified parent tree, so it is safe for local-directory tasks too.
	groups, identities := claudeTUIOwnedProcessGroups(cmd)
	claudeTUISignalCaptured(cmd, groups, identities, sig, expectedStart)
}

func claudeTUISignalCaptured(cmd *exec.Cmd, groups []int, identities map[int]map[int]uint64, sig syscall.Signal, expectedStart uint64) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	rootMatches := expectedStart == 0 || claudeTUIRootMatches(cmd, expectedStart)
	if rootMatches {
		claudeTUISignal(cmd, sig)
	}
	for _, p := range groups {
		if p == cmd.Process.Pid || p <= 1 {
			continue
		}
		if !claudeTUIGroupMatches(p, identities[p]) {
			continue
		}
		_ = syscall.Kill(-p, sig)
	}
}

func claudeTUIStop(cmd *exec.Cmd, opts ExecOptions, procDone <-chan struct{}, expectedStart uint64) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if expectedStart != 0 && !claudeTUIRootMatches(cmd, expectedStart) {
		return
	}
	groups, identities := claudeTUIOwnedProcessGroups(cmd)
	if expectedStart == 0 || claudeTUIRootMatches(cmd, expectedStart) {
		claudeTUISignal(cmd, syscall.SIGTERM)
	}
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		if claudeTUIProcessGone(cmd, procDone) && !claudeTUIGroupsAlive(groups) {
			return
		}
		select {
		case <-procDone:
			if !claudeTUIGroupsAlive(groups) {
				return
			}
			time.Sleep(25 * time.Millisecond)
		case <-deadline.C:
			if expectedStart == 0 || claudeTUIRootMatches(cmd, expectedStart) {
				claudeTUISignal(cmd, syscall.SIGKILL)
			}
			for _, pgid := range groups {
				if pgid > 1 {
					if claudeTUIGroupMatches(pgid, identities[pgid]) {
						_ = syscall.Kill(-pgid, syscall.SIGKILL)
					}
				}
			}
			return
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func claudeTUIRootMatches(cmd *exec.Cmd, expectedStart uint64) bool {
	if cmd == nil || cmd.Process == nil || expectedStart == 0 {
		return false
	}
	info, err := readCursorUnixProcessInfo(cmd.Process.Pid)
	return err == nil && !info.zombie && info.start == expectedStart
}

func claudeTUIProcessGone(cmd *exec.Cmd, procDone <-chan struct{}) bool {
	select {
	case <-procDone:
		return true
	default:
	}
	if cmd == nil || cmd.Process == nil {
		return true
	}
	return errors.Is(syscall.Kill(cmd.Process.Pid, 0), syscall.ESRCH)
}

func claudeTUIGroupsAlive(groups []int) bool {
	for _, pgid := range groups {
		if pgid > 1 && syscall.Kill(-pgid, 0) == nil {
			return true
		}
	}
	return false
}

func claudeTUIGroupMatches(pgid int, members map[int]uint64) bool {
	if pgid <= 1 || len(members) == 0 {
		return false
	}
	infos, err := listCursorUnixProcessInfos()
	if err != nil {
		return false
	}
	for _, info := range infos {
		if info.pgid == pgid {
			if start, ok := members[info.pid]; ok && start == info.start && !info.zombie {
				return true
			}
		}
	}
	return false
}

func waitClaudeTUIRead(done <-chan struct{}) {
	select {
	case <-done:
	case <-time.After(time.Second):
	}
}

// claudeTUIOwnedCwdProcesses finds Claude's descendants before the leader is
// signalled. It follows the live parent tree and never uses cwd matching.
func claudeTUIOwnedCwdProcesses(cmd *exec.Cmd, root string) []int {
	_ = root
	groups, _ := claudeTUIOwnedProcessGroups(cmd)
	return groups
}

func claudeTUIOwnedProcessGroups(cmd *exec.Cmd) ([]int, map[int]map[int]uint64) {
	infos, err := listCursorUnixProcessInfos()
	if err != nil || cmd == nil || cmd.Process == nil {
		return nil, nil
	}
	var rows []claudeTUIProc
	for _, info := range infos {
		rows = append(rows, claudeTUIProc{pid: info.pid, ppid: info.ppid, pgid: info.pgid, start: info.start})
	}
	owned := map[int]bool{cmd.Process.Pid: true}
	for changed := true; changed; {
		changed = false
		for _, row := range rows {
			if owned[row.ppid] && !owned[row.pid] {
				owned[row.pid] = true
				changed = true
			}
		}
	}
	groups := make(map[int]struct{})
	identities := make(map[int]map[int]uint64)
	for _, row := range rows {
		if !owned[row.pid] || row.pid == os.Getpid() || row.pid == cmd.Process.Pid || row.pgid <= 1 {
			continue
		}
		groups[row.pgid] = struct{}{}
		if identities[row.pgid] == nil {
			identities[row.pgid] = make(map[int]uint64)
		}
		identities[row.pgid][row.pid] = row.start
	}
	pids := make([]int, 0, len(groups))
	for pgid := range groups {
		pids = append(pids, pgid)
	}
	return pids, identities
}

type claudeTUIProc struct {
	pid, ppid, pgid int
	start           uint64
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
		st, err := f.Stat()
		if err != nil || off >= st.Size() {
			_ = f.Close()
			continue
		}
		data, err := io.ReadAll(io.NewSectionReader(f, off, st.Size()-off))
		if err != nil {
			_ = f.Close()
			continue
		}
		lastNL := bytes.LastIndexByte(data, '\n')
		if lastNL < 0 {
			// Keep the offset at the start of a row while Claude is still
			// writing it; the next poll will see the complete JSON object.
			_ = f.Close()
			continue
		}
		for _, line := range bytes.Split(data[:lastNL], []byte{'\n'}) {
			if len(line) > agentStreamMaxLineBytes {
				_ = f.Close()
				return fmt.Errorf("claude transcript row exceeds %d bytes", agentStreamMaxLineBytes)
			}
			var raw map[string]any
			if json.Unmarshal(line, &raw) != nil {
				continue
			}
			b.processTUIRow(raw, ch, usage, baseline, seen, finalText, finalErr, sawResult, terminalReason, promptRows, sessionID, expectedCwd)
		}
		offsets[path] = off + int64(lastNL+1)
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
		if sidechain, _ := raw["isSidechain"].(bool); sidechain {
			return
		}
		msg, _ := raw["message"].(map[string]any)
		if *promptRows == 0 {
			// Synthetic recovery assistant rows (for example, stop_sequence
			// after an interrupted tool) are not part of the new turn.
			if !isAPIErrorRow(raw) {
				return
			}
		}
		id, _ := msg["id"].(string)
		usageKey := ""
		if id != "" {
			usageKey = "usage:" + id
		}
		usageNew := true
		if usageKey != "" {
			_, usageNew = seen[usageKey]
			usageNew = !usageNew
		}
		if u, ok := msg["usage"].(map[string]any); ok && usageNew {
			if usageKey != "" {
				seen[usageKey] = struct{}{}
			}
			model, _ := msg["model"].(string)
			if model != "" {
				usage[model] = TokenUsage{InputTokens: int64(number(u["input_tokens"])), OutputTokens: int64(number(u["output_tokens"])), CacheReadTokens: int64(number(u["cache_read_input_tokens"])), CacheWriteTokens: int64(number(u["cache_creation_input_tokens"]))}
			}
		}
		apiError := isAPIErrorRow(raw)
		if id != "" && !apiError {
			marker := "final:" + id
			if _, exists := seen[marker]; !exists {
				for key := range seen {
					if strings.HasPrefix(key, "final:") {
						delete(seen, key)
					}
				}
				seen[marker] = struct{}{}
				*finalText = ""
			}
		}
		blocks, _ := msg["content"].([]any)
		for _, x := range blocks {
			bmap, _ := x.(map[string]any)
			typ, _ := bmap["type"].(string)
			switch typ {
			case "text":
				t, _ := bmap["text"].(string)
				if !apiError {
					if *finalText != "" && t != "" {
						*finalText += "\n"
					}
					*finalText += t
					trySend(ch, Message{Type: MessageText, Content: t})
				}
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
			*finalErr = tuiProviderErrorText(raw)
		}
	case "system":
		if sidechain, _ := raw["isSidechain"].(bool); sidechain {
			return
		}
		sub, _ := raw["subtype"].(string)
		if isAPIErrorRow(raw) {
			*finalErr = tuiProviderErrorText(raw)
		}
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

func tuiProviderErrorText(raw map[string]any) string {
	var parts []string
	if message, ok := raw["message"].(map[string]any); ok {
		if content, ok := message["content"].([]any); ok {
			for _, item := range content {
				if block, ok := item.(map[string]any); ok {
					for _, key := range []string{"text", "thinking"} {
						if text, ok := block[key].(string); ok && strings.TrimSpace(text) != "" {
							parts = append(parts, text)
						}
					}
				}
			}
		} else if text, ok := message["content"].(string); ok && strings.TrimSpace(text) != "" {
			parts = append(parts, text)
		}
	}
	for _, key := range []string{"errorDetails", "result", "error", "apiErrorStatus"} {
		if value, ok := raw[key]; ok {
			var text string
			switch v := value.(type) {
			case string:
				text = v
			default:
				if key == "result" {
					if data, err := json.Marshal(v); err == nil {
						text = string(data)
					}
				}
			}
			if strings.TrimSpace(text) != "" {
				duplicate := false
				for _, existing := range parts {
					if existing == text {
						duplicate = true
						break
					}
				}
				if !duplicate {
					parts = append(parts, text)
				}
			}
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, " | ")
	}
	data, _ := json.Marshal(raw)
	return string(data)
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
