package main

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
)

// accessState holds the console-menu-toggled switches that gate new AI
// work locally, without touching the wire protocol or aicmdd: pausing AI
// access entirely, or just blocking new exec starts (a panic button that
// doesn't interrupt already-running commands), plus a persistent set of
// env vars applied to commands this process spawns.
type accessState struct {
	aiEnabled      atomic.Bool
	newExecBlocked atomic.Bool

	mu  sync.Mutex
	env map[string]string
}

var access = newAccessState()

func newAccessState() *accessState {
	a := &accessState{env: map[string]string{}}
	a.aiEnabled.Store(true)
	return a
}

// checkExec returns a non-empty reason if a new exec/file_* request should
// be refused right now.
func (a *accessState) checkExec() string {
	if !a.aiEnabled.Load() {
		return "AI access is currently disabled from this console (menu: access on)"
	}
	if a.newExecBlocked.Load() {
		return "new commands are currently blocked from this console (menu: block off)"
	}
	return ""
}

// checkOther is checkExec without the new-exec block, for file_* requests
// (the block-new-exec panic button is specifically about commands, not
// file operations) -- only the AI-access-enabled toggle applies to those.
func (a *accessState) checkOther() string {
	if !a.aiEnabled.Load() {
		return "AI access is currently disabled from this console (menu: access on)"
	}
	return ""
}

// setEnv records a persistent env var, applied to future spawned processes
// (the persistent shell on its next restart, and every background one-shot
// command). The caller is responsible for also pushing it into any
// currently-live persistent shell for immediate effect -- see menu.go.
func (a *accessState) setEnv(key, value string) {
	a.mu.Lock()
	a.env[key] = value
	a.mu.Unlock()
}

func (a *accessState) unsetEnv(key string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.env[key]; !ok {
		return false
	}
	delete(a.env, key)
	return true
}

// environ returns base (typically os.Environ()) with the custom vars
// appended so they take precedence, in the "KEY=VALUE" form exec.Cmd.Env
// expects.
func (a *accessState) environ(base []string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.env) == 0 {
		return base
	}
	out := make([]string, len(base), len(base)+len(a.env))
	copy(out, base)
	for k, v := range a.env {
		out = append(out, k+"="+v)
	}
	return out
}

// listEnv returns the custom vars sorted by key, for the menu's status/env
// list commands.
func (a *accessState) listEnv() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	keys := make([]string, 0, len(a.env))
	for k := range a.env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	lines := make([]string, len(keys))
	for i, k := range keys {
		lines[i] = fmt.Sprintf("%s=%s", k, a.env[k])
	}
	return lines
}
