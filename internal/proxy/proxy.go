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
	"path/filepath"
	"sort"

	"ai-ssh/internal/paths"
)

// Discover resolves the socket of the target session. Order: explicit id,
// $AISH_SOCKET (set inside aish sessions), $AISH_SESSION, then scanning for
// live sockets — a single live one wins; with several, the most recently
// modified session dir wins. Stale sockets found while scanning are removed.
func Discover(explicitID string) (string, error) {
	if explicitID != "" {
		return checkLive(paths.Socket(explicitID))
	}
	if s := os.Getenv("AISH_SOCKET"); s != "" {
		return checkLive(s)
	}
	if id := os.Getenv("AISH_SESSION"); id != "" {
		return checkLive(paths.Socket(id))
	}

	entries, err := os.ReadDir(paths.Base())
	if err != nil {
		return "", fmt.Errorf("no aish sessions found (%s): is aish running?", paths.Base())
	}
	type cand struct {
		sock  string
		mtime int64
	}
	var live []cand
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sock := filepath.Join(paths.Base(), e.Name(), "mcp.sock")
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
		live = append(live, cand{sock, mt})
	}
	if len(live) == 0 {
		return "", fmt.Errorf("no live aish sessions found under %s", paths.Base())
	}
	sort.Slice(live, func(i, j int) bool { return live[i].mtime > live[j].mtime })
	return live[0].sock, nil
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
