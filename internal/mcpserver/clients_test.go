package mcpserver

import (
	"context"
	"strings"
	"testing"
	"time"

	"ai-ssh/internal/clientauth"
)

func TestConnectedClientsReportsIdentityAndState(t *testing.T) {
	approve := func(string, string, time.Duration) (byte, bool) { return 'y', true }
	ts := startTestServer(t, false, approve)
	cs := connectTestClient(t, ts.socket, "claude")
	identity, err := clientauth.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := identity.Authorize(context.Background(), cs, "testsession",
		"claude (via aish proxy launched by node)"); err != nil {
		t.Fatal(err)
	}

	clients := ts.core.ConnectedClients()
	if len(clients) != 1 {
		t.Fatalf("got %d clients, want 1: %+v", len(clients), clients)
	}
	got := clients[0]
	if got.Name != "claude" {
		t.Errorf("name = %q, want the MCP clientInfo name", got.Name)
	}
	if got.State != clientAuthorized {
		t.Errorf("state = %q, want %q", got.State, clientAuthorized)
	}
	if got.Description != "claude (via aish proxy launched by node)" {
		t.Errorf("declared identity = %q", got.Description)
	}
	if got.Since.IsZero() {
		t.Error("connection time was not stamped")
	}
	// The kernel-verified peer is the test binary over a Unix socket: present,
	// but not an aish process.
	if got.Peer == "" {
		t.Error("kernel-verified peer is missing")
	}
	if got.AishPeer {
		t.Error("the test binary must not be mistaken for an aish peer")
	}

	// The rendered view keeps the claimed and verified identities distinct.
	rendered := strings.Join(ts.core.ClientLines(), "\n")
	for _, want := range []string{"claude", clientAuthorized, "declared:", "verified:"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("client view is missing %q:\n%s", want, rendered)
		}
	}
}

// A connection that has not completed the handshake must read as pending, not
// as an authorized client.
func TestConnectedClientsShowsPendingBeforeApproval(t *testing.T) {
	blocked := func(string, string, time.Duration) (byte, bool) { return 0, false }
	ts := startTestServer(t, false, blocked)
	connectTestClient(t, ts.socket, "codex")

	clients := waitForClients(t, ts, 1)
	if clients[0].State != clientPending {
		t.Fatalf("state = %q, want %q", clients[0].State, clientPending)
	}
}

func TestClientCountMatchesConnections(t *testing.T) {
	ts := startTestServer(t, true, nil)
	if n := ts.core.ClientCount(); n != 0 {
		t.Fatalf("count with no clients = %d", n)
	}
	connectTestClient(t, ts.socket, "claude")
	waitForClients(t, ts, 1)
	if n := ts.core.ClientCount(); n != 1 {
		t.Fatalf("count = %d, want 1", n)
	}
}

func waitForClients(t *testing.T, ts *testServer, want int) []ConnectedClient {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		clients := ts.core.ConnectedClients()
		if len(clients) == want {
			return clients
		}
		if time.Now().After(deadline) {
			t.Fatalf("got %d clients, want %d", len(clients), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The version view exists to name one specific failure: a long-lived proxy
// still serving the tool schemas it loaded before an install. Only a client
// whose VERIFIED peer is an aish binary carries a comparable version, because
// the proxy renames its declared identity to the upstream TUI.
func TestVersionLineFlagsOnlyVerifiedAishMismatch(t *testing.T) {
	const session = "v0.2.2-42-gabcdef0"
	cases := []struct {
		name     string
		client   ConnectedClient
		wantFlag bool
	}{
		{
			name:     "stale aish proxy",
			client:   ConnectedClient{Name: "claude", Version: "v0.2.2-39-gc4f1bcb", AishPeer: true},
			wantFlag: true,
		},
		{
			name:     "matching aish proxy",
			client:   ConnectedClient{Name: "claude", Version: session, AishPeer: true},
			wantFlag: false,
		},
		{
			name:     "unrelated client with its own versioning",
			client:   ConnectedClient{Name: "some-editor", Version: "1.4.0", AishPeer: false},
			wantFlag: false,
		},
		{
			name:     "aish peer reporting no version",
			client:   ConnectedClient{Name: "claude", AishPeer: true},
			wantFlag: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line := versionClientLine(tc.client, session)
			if got := strings.Contains(line, "restart the AI client"); got != tc.wantFlag {
				t.Errorf("line %q: flagged=%v, want %v", line, got, tc.wantFlag)
			}
		})
	}
}

// Version is read by Serve when it builds the MCP server, so it must not be
// written after a server is running. A bare Core needs no server here anyway.
func TestVersionLinesWithoutClients(t *testing.T) {
	core := &Core{Version: "v0.2.2-42-gabcdef0"}
	lines := strings.Join(core.VersionLines(), "\n")
	if !strings.Contains(lines, "v0.2.2-42-gabcdef0") {
		t.Errorf("session version missing:\n%s", lines)
	}
	if !strings.Contains(lines, "no MCP clients are connected") {
		t.Errorf("expected the empty-client note:\n%s", lines)
	}
}

func TestShortDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{5 * time.Second, "5s"},
		{90 * time.Second, "2m"},
		{2*time.Hour + 13*time.Minute, "2h13m"},
	}
	for _, tc := range cases {
		if got := shortDuration(tc.in); got != tc.want {
			t.Errorf("shortDuration(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
