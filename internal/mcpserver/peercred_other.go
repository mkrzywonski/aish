//go:build !linux

package mcpserver

import "net"

// readUnixPeerCred has no portable implementation off Linux; the prompt then
// shows only the self-declared identity.
func readUnixPeerCred(*net.UnixConn) peerInfo { return peerInfo{} }
