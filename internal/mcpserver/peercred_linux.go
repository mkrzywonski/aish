//go:build linux

package mcpserver

import (
	"net"
	"syscall"
)

// readUnixPeerCred pulls {pid,uid,gid} off the socket via SO_PEERCRED. Reading
// credentials doesn't consume data, so it's safe alongside the MCP transport.
func readUnixPeerCred(uc *net.UnixConn) peerInfo {
	raw, err := uc.SyscallConn()
	if err != nil {
		return peerInfo{}
	}
	var (
		cred    *syscall.Ucred
		credErr error
	)
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil || credErr != nil || cred == nil {
		return peerInfo{}
	}
	return peerInfo{ok: true, pid: int(cred.Pid), uid: cred.Uid, gid: cred.Gid}
}
