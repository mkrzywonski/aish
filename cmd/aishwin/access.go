package main

import (
	"sort"
	"sync"
)

// accessState holds a persistent set of env vars applied to commands this
// process spawns. Access control now happens at the connection level (the
// Session > Clients... dialog's Disconnect button, gui_clients.go) rather
// than a global switch here -- an aiEnabled toggle lived here briefly, but
// nothing could turn it off (the Access menu that once did was removed) and
// disconnecting a client's connection is the finer-grained, actually-usable
// equivalent.
type accessState struct {
	mu  sync.Mutex
	env map[string]string
}

var access = newAccessState()

func newAccessState() *accessState {
	return &accessState{env: map[string]string{}}
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

// envEntry is one key/value pair, exposed for the Settings dialog's
// Environment tab list view (gui_settings.go).
type envEntry struct {
	Key   string
	Value string
}

// entries returns the custom vars sorted by key.
func (a *accessState) entries() []envEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	keys := make([]string, 0, len(a.env))
	for k := range a.env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]envEntry, len(keys))
	for i, k := range keys {
		out[i] = envEntry{Key: k, Value: a.env[k]}
	}
	return out
}
