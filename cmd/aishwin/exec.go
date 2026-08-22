package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"ai-ssh/internal/aishwinwire"
)

const defaultExecTimeout = 30 * time.Second

// execDispatcher wires incoming exec/exec_poll wire frames to the
// persistent shell (foreground) or the background task table (background).
type execDispatcher struct {
	kind shellKind

	mu    sync.Mutex // guards shell: swapped out for a fresh one after Run reports it dead
	shell *shellSession

	tasks *backgroundTasks
}

func newExecDispatcher(kind shellKind) *execDispatcher {
	return &execDispatcher{kind: kind, tasks: newBackgroundTasks()}
}

// currentShell returns a live persistent shell, starting a fresh one if
// there isn't one yet or the last one died (timed out or exited
// unexpectedly). A fresh shell loses cwd/env state from the old one — the
// same tradeoff internal/sshmux/channel.go accepts for a dead SSH channel.
// Freshly-started shells inherit the console menu's current custom env vars
// (access.environ) at spawn time.
func (d *execDispatcher) currentShell() (*shellSession, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.shell == nil || d.shell.dead.Load() {
		s, err := startShell(d.kind)
		if err != nil {
			return nil, err
		}
		d.shell = s
	}
	return d.shell, nil
}

// liveShell returns the current shell without starting one, or nil if none
// is running (or the last one died) — used by the menu to push a var into
// an already-running shell for immediate effect, where starting a fresh
// shell just to set a var would be surprising.
func (d *execDispatcher) liveShell() *shellSession {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.shell == nil || d.shell.dead.Load() {
		return nil
	}
	return d.shell
}

func (d *execDispatcher) handle(wc *aishwinwire.Conn, f aishwinwire.Frame) {
	switch f.Type {
	case "exec":
		d.handleExec(wc, f)
	case "exec_poll":
		d.handleExecPoll(wc, f)
	}
}

func (d *execDispatcher) handleExec(wc *aishwinwire.Conn, f aishwinwire.Frame) {
	var req aishwinwire.ExecData
	if err := json.Unmarshal(f.Data, &req); err != nil {
		return
	}

	// exec_poll (checking on an already-started task) is exempt: only
	// starting new work is gated, not observing what's already running.
	if reason := access.checkExec(); reason != "" {
		send(wc, "exec_result", f.ID, aishwinwire.ExecResultData{Error: reason})
		return
	}

	if req.Background {
		// Each background command gets its own fresh process (background.go),
		// which os/exec can point at a working directory natively via
		// cmd.Dir -- unlike the foreground path below, there's no need (and
		// real risk: embedding a quoted `cd /d "path" && ...` inside a
		// string that cmd.exe /c *also* quotes was corrupting paths with
		// spaces) to fold cwd into the command text here.
		id, err := d.tasks.Start(d.kind, req.Command, req.Cwd)
		result := aishwinwire.ExecResultData{TaskID: id}
		if err != nil {
			result = aishwinwire.ExecResultData{Error: err.Error()}
		}
		send(wc, "exec_result", f.ID, result)
		return
	}

	command := req.Command
	if req.Cwd != "" {
		// The persistent shell has no equivalent of cmd.Dir to set after the
		// fact, so this one-time cd has to be textual.
		command = withCwd(d.kind, req.Cwd, command)
	}

	shell, err := d.currentShell()
	if err != nil {
		send(wc, "exec_result", f.ID, aishwinwire.ExecResultData{Error: err.Error()})
		return
	}

	timeout := defaultExecTimeout
	if req.TimeoutMs > 0 {
		timeout = time.Duration(req.TimeoutMs) * time.Millisecond
	}

	output, exitCode, timedOut, err := shell.Run(command, timeout)
	result := aishwinwire.ExecResultData{Output: output, TimedOut: timedOut}
	if err != nil {
		result.Error = err.Error()
	} else if !timedOut {
		result.ExitCode = &exitCode
	}
	send(wc, "exec_result", f.ID, result)
}

func (d *execDispatcher) handleExecPoll(wc *aishwinwire.Conn, f aishwinwire.Frame) {
	var req aishwinwire.ExecPollData
	if err := json.Unmarshal(f.Data, &req); err != nil {
		return
	}
	running, output, next, code, err := d.tasks.Poll(req.TaskID, req.Cursor)
	result := aishwinwire.ExecPollResultData{Running: running, Output: output, NextCursor: next, ExitCode: code}
	if err != nil {
		result.Error = err.Error()
	}
	send(wc, "exec_poll_result", f.ID, result)
}

// withCwd folds a one-time working-directory change into command as a
// single line, so shellSession.Run's suffix-based echo detection (which
// expects one logical command) still sees exactly what was typed. Applies
// only to that one submission — the persistent shell's whole point is that
// state, cwd included, carries between commands, so this isn't restored
// afterward.
func withCwd(kind shellKind, cwd, command string) string {
	switch kind {
	case shellPowerShell:
		return fmt.Sprintf("Set-Location -LiteralPath '%s'; %s", strings.ReplaceAll(cwd, "'", "''"), command)
	default:
		return fmt.Sprintf(`cd /d "%s" && %s`, cwd, command)
	}
}

func send(wc *aishwinwire.Conn, frameType, id string, data any) {
	b, err := json.Marshal(data)
	if err != nil {
		return
	}
	_ = wc.Send(aishwinwire.Frame{Type: frameType, ID: id, Data: b})
}
