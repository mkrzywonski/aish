package mcpserver

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ai-ssh/internal/procinfo"
)

// Connection states reported to the human.
const (
	clientAuthorized = "authorized"
	clientPending    = "awaiting approval"
	clientDenied     = "denied"
	clientOpenGate   = "authorized (--no-auth)"
)

// ConnectedClient is one live MCP connection as the session sees it.
//
// It keeps the two identities apart the same way the approval prompt does, and
// for the same reason: Name, Version and Description are self-reported by the
// client and can say anything, while Peer comes from the kernel and cannot be
// forged. Through the proxy the verified peer is the proxy process rather than
// the AI product, which is exactly why the declared description exists.
type ConnectedClient struct {
	Name        string // MCP clientInfo name — what a grant binds to
	Version     string // MCP clientInfo version
	Description string // self-declared identity from request_access
	Peer        string // kernel-verified peer process
	AishPeer    bool   // the verified peer really is an aish binary
	State       string
	Since       time.Time
}

// ClientCount returns how many MCP connections are live, for the menu's
// at-a-glance count. Cheap enough to call while drawing the menu.
func (c *Core) ClientCount() int {
	c.authMu.Lock()
	defer c.authMu.Unlock()
	return len(c.conns)
}

// ConnectedClients returns a snapshot of the live MCP connections, oldest
// first.
func (c *Core) ConnectedClients() []ConnectedClient {
	// Collect the connections under authMu, then read each one's own state
	// after releasing it. requestAccess holds st.mu and then takes authMu, so
	// holding authMu across st.mu would invert that order and deadlock.
	c.authMu.Lock()
	sessions := make([]*mcp.ServerSession, 0, len(c.conns))
	states := make([]*connAuth, 0, len(c.conns))
	for ss, st := range c.conns {
		sessions = append(sessions, ss)
		states = append(states, st)
	}
	c.authMu.Unlock()

	out := make([]ConnectedClient, 0, len(states))
	for i, st := range states {
		st.mu.Lock()
		client := ConnectedClient{
			Description: st.declared,
			Peer:        st.peer.String(),
			AishPeer:    st.peer.isAish(),
			Since:       st.since,
		}
		switch {
		case c.NoAuth:
			client.State = clientOpenGate
		case st.grantID != "":
			client.State = clientAuthorized
		case st.denied:
			client.State = clientDenied
		default:
			client.State = clientPending
		}
		st.mu.Unlock()

		if ss := sessions[i]; ss != nil {
			if ip := ss.InitializeParams(); ip != nil && ip.ClientInfo != nil {
				client.Name = ip.ClientInfo.Name
				client.Version = ip.ClientInfo.Version
			}
		}
		if client.Name == "" {
			client.Name = "an MCP client"
		}
		out = append(out, client)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out
}

// isAish reports whether the kernel-verified peer is an aish binary. This is
// the trustworthy half of the identity: the proxy renames its own MCP
// clientInfo to the upstream TUI ("claude", "codex"), so the declared name
// cannot tell you whether a version string is an aish build version, and the
// peer can.
func (p peerInfo) isAish() bool {
	return p.ok && procinfo.Comm(p.pid) == "aish"
}

// ClientLines renders the connected clients for the Ctrl-] menu.
func (c *Core) ClientLines() []string {
	clients := c.ConnectedClients()
	if len(clients) == 0 {
		return nil
	}
	lines := make([]string, 0, len(clients)*3)
	for _, client := range clients {
		head := client.Name
		if client.Version != "" {
			head += " " + client.Version
		}
		head += "  —  " + client.State
		if !client.Since.IsZero() {
			head += ", connected " + shortDuration(time.Since(client.Since)) + " ago"
		}
		lines = append(lines, head)
		if client.Description != "" && client.Description != client.Name {
			lines = append(lines, "    declared: "+client.Description)
		}
		if client.Peer != "" {
			lines = append(lines, "    verified: "+client.Peer)
		}
	}
	return lines
}

// VersionLines renders this session's build and every connected client's, for
// the Ctrl-] menu.
//
// The comparison is the point rather than the numbers. A long-lived AI proxy
// keeps the tool schemas and server instructions it loaded at startup, so after
// an install it can be talking to a newer session with an older idea of what
// that session offers — a confusing failure that looks like a broken tool. A
// client whose verified peer is an aish binary carries a comparable version, so
// that specific mismatch can be named instead of guessed at.
func (c *Core) VersionLines() []string {
	version := c.Version
	if version == "" {
		version = "unknown"
	}
	lines := []string{"this session:  " + version}
	if exe, err := os.Executable(); err == nil {
		lines = append(lines, "    running:   "+exe)
	}

	clients := c.ConnectedClients()
	if len(clients) == 0 {
		return append(lines, "no MCP clients are connected")
	}
	lines = append(lines, "connected clients (versions are self-reported):")
	for _, client := range clients {
		lines = append(lines, versionClientLine(client, c.Version))
	}
	return lines
}

// versionClientLine renders one client's version, flagging a mismatch only when
// the version is actually comparable: the peer must be a verified aish binary,
// since any other client versions its own product on its own scheme.
func versionClientLine(client ConnectedClient, sessionVersion string) string {
	reported := client.Version
	if reported == "" {
		reported = "no version reported"
	}
	line := "    " + client.Name + "  " + reported
	if client.AishPeer && client.Version != "" && client.Version != sessionVersion {
		line += "  <- differs from this session; restart the AI client so it reloads aish's tools"
	}
	return line
}

// shortDuration renders a connection age compactly: seconds under a minute,
// then minutes, then hours — a menu wants "2h13m", not "2h13m7.4s".
func shortDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return strings.TrimSuffix(d.Round(time.Minute).String(), "0s")
	default:
		return strings.TrimSuffix(d.Round(time.Minute).String(), "0s")
	}
}

// statusClient is one live MCP connection as session_status reports it.
//
// Sessions are shared on purpose here — that is the reason oob_log exists —
// but nothing told an AI who else was attached. An evaluation agent inferred a
// second actor from console timestamps it could not otherwise explain and had
// to report the conclusion as unconfirmed, because no tool would say. The
// human has had this all along in the Ctrl-] menu.
//
// Declared identity and verified peer stay separate fields for the same reason
// the menu keeps them visually distinct: the description is self-reported and
// spoofable, the peer is kernel-verified.
type statusClient struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Peer        string `json:"peer,omitempty"`
	State       string `json:"state"`
	Since       string `json:"since"`
	Self        bool   `json:"self,omitempty"` // this connection, so "who else" is answerable
}

// statusClients snapshots the live connections for session_status.
func (c *Core) statusClients() []statusClient {
	clients := c.ConnectedClients()
	if len(clients) == 0 {
		return nil
	}
	out := make([]statusClient, 0, len(clients))
	for _, cl := range clients {
		out = append(out, statusClient{
			Name:        cl.Name,
			Description: cl.Description,
			Peer:        cl.Peer,
			State:       cl.State,
			Since:       cl.Since.Format(time.RFC3339),
		})
	}
	return out
}
