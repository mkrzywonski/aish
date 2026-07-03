// Package proxy implements `aish mcp-proxy`: a dumb stdio<->Unix-socket byte
// pump that lets stdio-only MCP clients (Claude Code, Codex) talk to a
// running aish session. It does not parse MCP; framing is newline-delimited
// JSON on both sides.
package proxy

import (
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strings"

	"ai-ssh/internal/paths"
)

// SessionInfo describes one live session found on this machine.
type SessionInfo struct {
	ID    string
	Name  string // "" when unnamed
	Sock  string
	MTime int64 // session dir mtime, unix nanos
}

// Label renders the session for user-facing listings.
func (s SessionInfo) Label() string {
	if s.Name != "" {
		return s.ID + " (" + s.Name + ")"
	}
	return s.ID
}

// List scans the runtime base dir for live sessions, sorted by id.
// Stale sockets found while scanning are removed.
func List() []SessionInfo {
	entries, err := os.ReadDir(paths.Base())
	if err != nil {
		return nil
	}
	var live []SessionInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sock := paths.Socket(e.Name())
		if err := ping(sock); err != nil {
			// Stale leftover from a killed session; clean it up.
			os.Remove(sock)
			continue
		}
		info, _ := e.Info()
		var mt int64
		if info != nil {
			mt = info.ModTime().UnixNano()
		}
		live = append(live, SessionInfo{ID: e.Name(), Name: paths.ReadName(e.Name()), Sock: sock, MTime: mt})
	}
	sort.Slice(live, func(i, j int) bool { return live[i].ID < live[j].ID })
	return live
}

// Resolve picks the session matching target — exact id, unique name, or
// unique id prefix — from live.
func Resolve(target string, live []SessionInfo) (SessionInfo, error) {
	var byName, byPrefix []SessionInfo
	for _, s := range live {
		if s.ID == target {
			return s, nil
		}
		if s.Name != "" && s.Name == target {
			byName = append(byName, s)
		}
		if strings.HasPrefix(s.ID, target) {
			byPrefix = append(byPrefix, s)
		}
	}
	switch {
	case len(byName) == 1:
		return byName[0], nil
	case len(byName) > 1:
		return SessionInfo{}, fmt.Errorf("several sessions are named %q: %s — use the id", target, labels(byName))
	case len(byPrefix) == 1:
		return byPrefix[0], nil
	case len(byPrefix) > 1:
		return SessionInfo{}, fmt.Errorf("session %q is ambiguous: %s", target, labels(byPrefix))
	}
	return SessionInfo{}, fmt.Errorf("no session matches %q; live sessions: %s", target, labels(live))
}

// Discover resolves the socket of the target session. target (from --session
// or $AISH_SESSION) may be a session id, a session name, or a unique id
// prefix. Without a target: $AISH_SOCKET (set inside aish sessions) wins;
// otherwise the most recently active live session is used. Attaching is not
// exclusive: every other session stays reachable through the tools' session
// argument, so a default pick is safe.
func Discover(target string) (string, error) {
	if target == "" {
		if s := os.Getenv("AISH_SOCKET"); s != "" {
			return checkLive(s)
		}
		target = os.Getenv("AISH_SESSION")
	}
	live := List()
	if len(live) == 0 {
		return "", fmt.Errorf("no live aish sessions found under %s: is aish running?", paths.Base())
	}

	if target == "" {
		newest := live[0]
		for _, s := range live[1:] {
			if s.MTime > newest.MTime {
				newest = s
			}
		}
		return newest.Sock, nil
	}

	s, err := Resolve(target, live)
	if err != nil {
		return "", err
	}
	return s.Sock, nil
}

func labels(ss []SessionInfo) string {
	parts := make([]string, len(ss))
	for i, s := range ss {
		parts[i] = s.Label()
	}
	return strings.Join(parts, ", ")
}

func checkLive(sock string) (string, error) {
	if err := ping(sock); err != nil {
		return "", fmt.Errorf("session socket %s not reachable: %w", sock, err)
	}
	return sock, nil
}

func ping(sock string) error {
	c, err := net.Dial("unix", sock)
	if err != nil {
		return err
	}
	c.Close()
	return nil
}

// Main runs the proxy until either side closes.
func Main(args []string) int {
	var sessionID string
	for i := 0; i < len(args); i++ {
		if args[i] == "--session" && i+1 < len(args) {
			sessionID = args[i+1]
			i++
		}
	}
	sock, err := Discover(sessionID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "aish mcp-proxy:", err)
		return 1
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		fmt.Fprintln(os.Stderr, "aish mcp-proxy:", err)
		return 1
	}
	defer conn.Close()

	done := make(chan struct{}, 2)
	go func() {
		io.Copy(conn, os.Stdin)
		if uc, ok := conn.(*net.UnixConn); ok {
			uc.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		io.Copy(os.Stdout, conn)
		done <- struct{}{}
	}()
	<-done
	return 0
}
