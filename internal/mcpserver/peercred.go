package mcpserver

import (
	"fmt"
	"net"

	"ai-ssh/internal/procinfo"
)

// peerInfo is the kernel-verified identity of the process on the other end of a
// session's Unix socket (from SO_PEERCRED). Unlike the client's self-declared
// name/description, the peer cannot forge this. Through the aish proxy the peer
// is the proxy itself, so this names the local relay, not the AI product — it
// confirms a same-uid local process and flags anything unexpected.
type peerInfo struct {
	ok  bool
	pid int
	uid uint32
	gid uint32
}

// String renders the verified peer for the approval prompt, e.g.
// "aish (pid 4521, uid 1000)". Empty when creds are unavailable (non-Linux, or
// a transport without peer credentials).
func (p peerInfo) String() string {
	if !p.ok {
		return ""
	}
	name := procinfo.Name(p.pid)
	if name == "" {
		name = "process"
	}
	return fmt.Sprintf("%s (pid %d, uid %d)", name, p.pid, p.uid)
}

// peerCred reads the peer credentials of a Unix-socket connection. Non-Unix
// transports (or non-Linux builds) yield an empty peerInfo, so callers degrade
// to showing only the self-declared identity.
func peerCred(conn net.Conn) peerInfo {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return peerInfo{}
	}
	return readUnixPeerCred(uc)
}
