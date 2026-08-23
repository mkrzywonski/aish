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

// execDispatcher wires incoming exec/exec_poll wire frames to a persistent
// shell (foreground) or the background task table (background). It is not
// locked to one fixed shellKind for its whole lifetime: each supported kind
// (cmd, powershell, and bash if available) gets its own independently
// lazily-started, independently-dying persistent shell, so the AI can pick
// whichever fits a given command without losing another kind's cwd/env
// state by switching to it.
type execDispatcher struct {
	defaultKind shellKind   // used when a request doesn't specify shell
	available   []shellKind // kinds this host can actually run (bash may be absent)

	mu     sync.Mutex // guards shells: one entry swapped out after Run reports it dead
	shells map[shellKind]*shellSession

	tasks *backgroundTasks
}

func newExecDispatcher(defaultKind shellKind, available []shellKind) *execDispatcher {
	return &execDispatcher{
		defaultKind: defaultKind,
		available:   available,
		shells:      map[shellKind]*shellSession{},
		tasks:       newBackgroundTasks(),
	}
}

func (d *execDispatcher) isAvailable(kind shellKind) bool {
	for _, k := range d.available {
		if k == kind {
			return true
		}
	}
	return false
}

// resolveKind maps an exec call's (possibly empty) requested shell name to
// a validated shellKind, applying d.defaultKind when unspecified. Rejects
// both a genuinely unknown name and a recognized-but-unavailable one (bash,
// most commonly) up front with an actionable message, rather than trying
// and failing obscurely once startShell itself can't find the binary.
func (d *execDispatcher) resolveKind(requested string) (shellKind, error) {
	kind := d.defaultKind
	if requested != "" {
		kind = shellKind(requested)
	}
	switch kind {
	case shellCmd, shellPowerShell, shellBash:
	default:
		return "", fmt.Errorf("unknown shell %q (use cmd, powershell, or bash)", requested)
	}
	if !d.isAvailable(kind) {
		return "", fmt.Errorf("shell %q is not available on this host (available: %s)", kind, strings.Join(shellKindStrings(d.available), ", "))
	}
	return kind, nil
}

// currentShell returns a live persistent shell of the given kind, starting
// a fresh one if there isn't one yet or the last one died (timed out or
// exited unexpectedly). A fresh shell loses cwd/env state from the old one
// — the same tradeoff internal/sshmux/channel.go accepts for a dead SSH
// channel — but never touches any OTHER kind's shell. Freshly-started
// shells inherit the console menu's current custom env vars
// (access.environ) at spawn time.
func (d *execDispatcher) currentShell(kind shellKind) (*shellSession, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.shells[kind]
	if s == nil || s.dead.Load() {
		fresh, err := startShell(kind)
		if err != nil {
			return nil, err
		}
		d.shells[kind] = fresh
		s = fresh
	}
	return s, nil
}

// liveShells returns every currently-live persistent shell across all
// kinds — used by the console menu's env-var push (realmenu.go:
// pushLiveEnv) to apply a newly-set var to whichever shells are actually
// running right now, since more than one kind can be alive at once.
func (d *execDispatcher) liveShells() []*shellSession {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []*shellSession
	for _, s := range d.shells {
		if s != nil && !s.dead.Load() {
			out = append(out, s)
		}
	}
	return out
}

// liveCWD returns kind's persistent shell's last-known cwd, or "" if that
// kind has no live shell yet. Used to default a background exec call's cwd
// when the caller didn't specify one: background commands get their own
// fresh process (background.go) with no persistent-shell state of their
// own to fall back on otherwise, which previously (before per-kind shells
// existed) meant an unspecified cwd silently resolved to this process's
// own launch directory instead of "wherever the AI thinks it's working" —
// a real, confusing mismatch found live (a `go build` failed with a
// misleading "not a module" error instead of an obvious cwd-mismatch
// signal). Keyed by kind: a background bash call inherits bash's own last
// cwd, not cmd's or powershell's.
func (d *execDispatcher) liveCWD(kind shellKind) string {
	d.mu.Lock()
	s := d.shells[kind]
	d.mu.Unlock()
	if s == nil || s.dead.Load() {
		return ""
	}
	return s.CWD()
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

	kind, err := d.resolveKind(req.Shell)
	if err != nil {
		send(wc, "exec_result", f.ID, aishwinwire.ExecResultData{Error: err.Error()})
		return
	}

	if req.Background {
		// Each background command gets its own fresh process (background.go),
		// which os/exec can point at a working directory natively via
		// cmd.Dir -- unlike the foreground path below, there's no need (and
		// real risk: embedding a quoted `cd /d "path" && ...` inside a
		// string that cmd.exe /c *also* quotes was corrupting paths with
		// spaces) to fold cwd into the command text here.
		//
		// An unspecified cwd defaults to this kind's own persistent shell's
		// last-known cwd (liveCWD), not this process's own launch
		// directory -- matching what the AI actually expects ("wherever
		// that shell currently is") instead of silently resolving
		// somewhere unrelated.
		cwd := req.Cwd
		if cwd == "" {
			cwd = d.liveCWD(kind)
		}
		id, err := d.tasks.Start(kind, req.Command, cwd)
		result := aishwinwire.ExecResultData{TaskID: id, Shell: string(kind)}
		if err != nil {
			result = aishwinwire.ExecResultData{Error: err.Error(), Shell: string(kind)}
		}
		send(wc, "exec_result", f.ID, result)
		return
	}

	command := req.Command
	if req.Cwd != "" {
		// The persistent shell has no equivalent of cmd.Dir to set after the
		// fact, so this one-time cd has to be textual.
		command = withCwd(kind, req.Cwd, command)
	}

	shell, err := d.currentShell(kind)
	if err != nil {
		send(wc, "exec_result", f.ID, aishwinwire.ExecResultData{Error: err.Error(), Shell: string(kind)})
		return
	}

	timeout := defaultExecTimeout
	if req.TimeoutMs > 0 {
		timeout = time.Duration(req.TimeoutMs) * time.Millisecond
	}

	output, exitCode, timedOut, err := shell.Run(command, timeout)
	result := aishwinwire.ExecResultData{Output: output, TimedOut: timedOut, Shell: string(kind)}
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
	case shellBash:
		return fmt.Sprintf("cd %s && %s", bashSingleQuote(cwd), command)
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
