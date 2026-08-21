package main

import (
	"sync"

	"ai-ssh/internal/aicmdwire"
)

// runtimeState is connection/session info the menu (menu.go) needs to
// reach across from link.go's per-connection scope: the current wire link
// (for sending a menu-originated request like rename), and what aicmdd told
// us about the session in its hello_ack.
type runtimeState struct {
	mu            sync.Mutex
	wire          *aicmdwire.Conn
	connected     bool
	sessionID     string
	name          string
	aicmddVersion string
}

var rt = &runtimeState{}

func (r *runtimeState) setConnected(wc *aicmdwire.Conn, ack aicmdwire.HelloAckData) {
	r.mu.Lock()
	r.wire = wc
	r.connected = true
	r.sessionID = ack.SessionID
	r.name = ack.Name
	r.aicmddVersion = ack.Version
	r.mu.Unlock()
}

func (r *runtimeState) setDisconnected() {
	r.mu.Lock()
	r.connected = false
	r.mu.Unlock()
}

func (r *runtimeState) setName(name string) {
	r.mu.Lock()
	r.name = name
	r.mu.Unlock()
}

type runtimeSnapshot struct {
	wire          *aicmdwire.Conn
	connected     bool
	sessionID     string
	name          string
	aicmddVersion string
}

func (r *runtimeState) snapshot() runtimeSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return runtimeSnapshot{
		wire: r.wire, connected: r.connected,
		sessionID: r.sessionID, name: r.name, aicmddVersion: r.aicmddVersion,
	}
}
