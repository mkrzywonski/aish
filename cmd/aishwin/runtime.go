//go:build windows

package main

import (
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
	connMode       string // connModeWSL or connModeSSH -- see connDescriptor (spawn.go)
	connTarget     string
}

var rt = &runtimeState{}

// setConnDescriptor records what StartConnection (connection.go) is about
// to connect to -- called immediately, before the connection attempt
// itself, so the status bar's tooltip (gui_statusbar.go) can describe a
// reconnect-in-progress the same way it describes an already-connected
// link.
func (r *runtimeState) setConnDescriptor(desc connDescriptor) {
	r.mu.Lock()
	r.connMode = desc.mode
	r.connTarget = desc.target
	r.mu.Unlock()
}

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

// refreshStatus updates the graphical status bar's connected LED (see
// gui_statusbar.go) and session-name item to match runtimeState's current
// values. Safe to call before the window exists -- SetConnected/
// updateSessionNameDisplay queue their work and no-op safely until
// hwndStatus is set.
func refreshStatus() {
	snap := rt.snapshot()
	SetConnected(snap.connected)
	updateSessionNameDisplay(snap.name)
}

type runtimeSnapshot struct {
	wire           *aishwinwire.Conn
	connected      bool
	sessionID      string
	name           string
	aishwndVersion string
	connMode       string
	connTarget     string
}

func (r *runtimeState) snapshot() runtimeSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return runtimeSnapshot{
		wire: r.wire, connected: r.connected,
		sessionID: r.sessionID, name: r.name, aishwndVersion: r.aishwndVersion,
		connMode: r.connMode, connTarget: r.connTarget,
	}
}
