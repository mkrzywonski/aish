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
	ID   string
	Name string // "" when unnamed
	Sock string
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
		live = append(live, SessionInfo{ID: e.Name(), Name: paths.ReadName(e.Name()), Sock: sock})
	}
	sort.Slice(live, func(i, j int) bool { return live[i].ID < live[j].ID })
	return live
}

// Discover resolves the socket of the target session. target (from --session
// or $AISH_SESSION) may be a session id, a session name, or a unique id
// prefix. Without a target: $AISH_SOCKET (set inside aish sessions) wins,
// then a scan — a single live session is used; several is an error listing
// them, so callers are never silently attached to the wrong terminal.
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
		if len(live) == 1 {
			return live[0].Sock, nil
		}
		return "", fmt.Errorf("multiple aish sessions are live: %s — pick one with --session <id|name> or AISH_SESSION",
			labels(live))
	}

	var byName, byPrefix []SessionInfo
	for _, s := range live {
		if s.ID == target {
			return s.Sock, nil
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
		return byName[0].Sock, nil
	case len(byName) > 1:
		return "", fmt.Errorf("several sessions are named %q: %s — use the id", target, labels(byName))
	case len(byPrefix) == 1:
		return byPrefix[0].Sock, nil
	case len(byPrefix) > 1:
		return "", fmt.Errorf("session %q is ambiguous: %s", target, labels(byPrefix))
	}
	return "", fmt.Errorf("no session matches %q; live sessions: %s", target, labels(live))
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
