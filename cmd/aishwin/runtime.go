package main

import (
	"fmt"
	"os"
	"sync"

	"ai-ssh/internal/aishwinwire"
)

// runtimeState is connection/session info the menu (menu.go) needs to
// reach across from link.go's per-connection scope: the current wire link
// (for sending a menu-originated request like rename), and what aishwnd told
// us about the session in its hello_ack.
type runtimeState struct {
	mu             sync.Mutex
	wire           *aishwinwire.Conn
	connected      bool
	sessionID      string
	name           string
	aishwndVersion string
}

var rt = &runtimeState{}

func (r *runtimeState) setConnected(wc *aishwinwire.Conn, ack aishwinwire.HelloAckData) {
	r.mu.Lock()
	r.wire = wc
	r.connected = true
	r.sessionID = ack.SessionID
	r.name = ack.Name
	r.aishwndVersion = ack.Version
	r.mu.Unlock()
	refreshStatus()
}

func (r *runtimeState) setDisconnected() {
	r.mu.Lock()
	r.connected = false
	r.mu.Unlock()
	refreshStatus()
}

func (r *runtimeState) setName(name string) {
	r.mu.Lock()
	r.name = name
	r.mu.Unlock()
	refreshStatus()
}

// refreshStatus renders the current connection state into the GUI status
// bar. Safe to call before the window exists (SetStatus/AppendLog no-op
// until hwndMain is set).
func refreshStatus() {
	snap := rt.snapshot()
	state := "disconnected"
	label := "none"
	if snap.connected {
		state = "connected"
		label = snap.sessionID
		if snap.name != "" {
			label = fmt.Sprintf("%s (%s)", snap.sessionID, snap.name)
		}
	}
	// pid is shown so a screenshot alone identifies which running
	// aishwin.exe instance it is -- otherwise indistinguishable when
	// several are running at once (e.g. a production session alongside a
	// throwaway test instance) other than by cross-referencing
	// Get-CimInstance/tasklist by hand. buildStampStr's time (hostname
	// omitted here for space -- Help>About has the full stamp) is what
	// actually answers "is this the build I just made": the version string
	// alone is identical for every build of the same commit. Shell/AI
	// access/blocked used to live only in the now-removed Help>Status
	// dialog -- folded in here instead, since it's state the user wants to
	// glance at, not look up.
	text := fmt.Sprintf("%s  |  pid: %d  |  session: %s  |  shell: %s  |  AI access: %s  |  blocked: %s  |  aishwin %s (%s)",
		state, os.Getpid(), label, execD.kind, onOff(access.aiEnabled.Load()), onOff(access.newExecBlocked.Load()), version, buildTimeOnly())
	if snap.connected && snap.aishwndVersion != "" {
		text += fmt.Sprintf("  |  aishwnd %s", snap.aishwndVersion)
	}
	SetStatus(text)
}

type runtimeSnapshot struct {
	wire           *aishwinwire.Conn
	connected      bool
	sessionID      string
	name           string
	aishwndVersion string
}

func (r *runtimeState) snapshot() runtimeSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return runtimeSnapshot{
		wire: r.wire, connected: r.connected,
		sessionID: r.sessionID, name: r.name, aishwndVersion: r.aishwndVersion,
	}
}
