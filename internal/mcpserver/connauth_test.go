package mcpserver

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ai-ssh/internal/authproto"
	"ai-ssh/internal/clientauth"
	"ai-ssh/internal/paths"
	"ai-ssh/internal/session"
)

type testServer struct {
	core   *Core
	socket string
	cancel context.CancelFunc
}

func startTestServer(t *testing.T, noAuth bool, prompt func(string, string, time.Duration) (byte, bool)) *testServer {
	t.Helper()
	runtimeDir, err := os.MkdirTemp("/tmp", "aish-auth-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(runtimeDir) })
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	id := "testsession"
	if err := os.MkdirAll(paths.SessionDir(id), 0o700); err != nil {
		t.Fatal(err)
	}
	core := &Core{
		Sess: session.New(id, []string{"/bin/sh"}, nil), Version: "test", NoAuth: noAuth,
		ApprovalPrompt: prompt, ApprovalTimeout: time.Second, ChallengeTTL: 20 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	socket := paths.Socket(id)
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, core, socket) }()
	deadline := time.Now().Add(time.Second)
	for {
		conn, err := net.Dial("unix", socket)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("server did not listen: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("Serve did not stop")
		}
	})
	return &testServer{core: core, socket: socket, cancel: cancel}
}

func connectTestClient(t *testing.T, socket, name string) *mcp.ClientSession {
	t.Helper()
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: name, Version: "test"}, nil)
	cs, err := client.Connect(context.Background(), &mcp.IOTransport{Reader: conn, Writer: conn}, nil)
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cs.Close()
		conn.Close()
	})
	return cs
}

func callTool(t *testing.T, cs *mcp.ClientSession, name string, args any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func decodeResult[T any](t *testing.T, res *mcp.CallToolResult) T {
	t.Helper()
	var out T
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestApprovalGrantsAndReconnect(t *testing.T) {
	var prompts atomic.Int32
	ts := startTestServer(t, false, func(string, string, time.Duration) (byte, bool) {
		prompts.Add(1)
		return 'y', true
	})

	cs := connectTestClient(t, ts.socket, "client-one")
	if res := callTool(t, cs, "set_session_name", map[string]any{"name": "before-auth"}); !res.IsError {
		t.Fatal("unauthorized tool call succeeded")
	}
	identity, err := clientauth.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := identity.Authorize(context.Background(), cs, ts.core.Sess.ID, "test client"); err != nil {
		t.Fatal(err)
	}
	if res := callTool(t, cs, "set_session_name", map[string]any{"name": "approved"}); res.IsError {
		t.Fatalf("authorized tool call failed: %#v", res.Content)
	}
	if got := prompts.Load(); got != 1 {
		t.Fatalf("prompts = %d, want 1", got)
	}
	cs.Close()

	cs2 := connectTestClient(t, ts.socket, "client-one")
	if err := identity.Authorize(context.Background(), cs2, ts.core.Sess.ID, "test client"); err != nil {
		t.Fatal(err)
	}
	if got := prompts.Load(); got != 1 {
		t.Fatalf("reconnect prompted again: prompts = %d", got)
	}

	identity2, err := clientauth.New()
	if err != nil {
		t.Fatal(err)
	}
	cs3 := connectTestClient(t, ts.socket, "client-two")
	if err := identity2.Authorize(context.Background(), cs3, ts.core.Sess.ID, "test client"); err != nil {
		t.Fatal(err)
	}
	if got := prompts.Load(); got != 2 {
		t.Fatalf("second client prompts = %d, want 2", got)
	}
	ts.core.authMu.Lock()
	grants := len(ts.core.grants)
	ts.core.authMu.Unlock()
	if grants != 2 {
		t.Fatalf("grants = %d, want 2", grants)
	}
	if _, err := os.Stat(filepath.Join(paths.SessionDir(ts.core.Sess.ID), "token")); !os.IsNotExist(err) {
		t.Fatalf("legacy token file exists or stat failed unexpectedly: %v", err)
	}
}

func TestApprovalPromptShowsIdentityAndVerifiedPeer(t *testing.T) {
	var captured atomic.Value // string
	ts := startTestServer(t, false, func(q string, _ string, _ time.Duration) (byte, bool) {
		captured.Store(q)
		return 'y', true
	})
	cs := connectTestClient(t, ts.socket, "raw-client-info")

	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	res := callTool(t, cs, authproto.RequestAccessTool, authproto.RequestAccessArgs{
		PublicKey:         base64.RawURLEncoding.EncodeToString(public),
		ClientDescription: "Gemini (Antigravity)",
	})
	if res.IsError {
		t.Fatalf("request_access errored: %#v", res.Content)
	}

	q, _ := captured.Load().(string)
	if !strings.Contains(q, "Gemini (Antigravity)") {
		t.Errorf("prompt missing self-declared identity: %q", q)
	}
	// The test client connects over the same Unix socket, so SO_PEERCRED
	// resolves to this test process — the prompt must carry a verified line.
	if !strings.Contains(q, "verified:") || !strings.Contains(q, "pid ") {
		t.Errorf("prompt missing verified peer: %q", q)
	}
}

func TestChallengeIsBoundSingleUseAndExpiring(t *testing.T) {
	ts := startTestServer(t, false, func(string, string, time.Duration) (byte, bool) { return 'y', true })
	cs := connectTestClient(t, ts.socket, "client-one")
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	request := callTool(t, cs, authproto.RequestAccessTool, authproto.RequestAccessArgs{
		PublicKey: base64.RawURLEncoding.EncodeToString(public),
	})
	grant := decodeResult[authproto.RequestAccessResult](t, request)
	cs.Close()

	cs2 := connectTestClient(t, ts.socket, "client-one")
	challengeRes := callTool(t, cs2, authproto.ChallengeTool, authproto.ChallengeArgs{GrantID: grant.GrantID})
	challenge := decodeResult[authproto.ChallengeResult](t, challengeRes)
	_, wrongPrivate, _ := ed25519.GenerateKey(rand.Reader)
	wrongSig := ed25519.Sign(wrongPrivate, authproto.SigningPayload(challenge.SessionID, grant.GrantID, challenge.ChallengeID, challenge.Nonce))
	bad := callTool(t, cs2, authproto.AuthenticateTool, authproto.AuthenticateArgs{
		GrantID: grant.GrantID, ChallengeID: challenge.ChallengeID,
		Signature: base64.RawURLEncoding.EncodeToString(wrongSig),
	})
	if !bad.IsError {
		t.Fatal("wrong key was accepted")
	}
	goodSig := ed25519.Sign(private, authproto.SigningPayload(challenge.SessionID, grant.GrantID, challenge.ChallengeID, challenge.Nonce))
	replay := callTool(t, cs2, authproto.AuthenticateTool, authproto.AuthenticateArgs{
		GrantID: grant.GrantID, ChallengeID: challenge.ChallengeID,
		Signature: base64.RawURLEncoding.EncodeToString(goodSig),
	})
	if !replay.IsError {
		t.Fatal("consumed challenge was replayed")
	}

	expiring := decodeResult[authproto.ChallengeResult](t,
		callTool(t, cs2, authproto.ChallengeTool, authproto.ChallengeArgs{GrantID: grant.GrantID}))
	time.Sleep(30 * time.Millisecond)
	expiredSig := ed25519.Sign(private, authproto.SigningPayload(expiring.SessionID, grant.GrantID, expiring.ChallengeID, expiring.Nonce))
	expired := callTool(t, cs2, authproto.AuthenticateTool, authproto.AuthenticateArgs{
		GrantID: grant.GrantID, ChallengeID: expiring.ChallengeID,
		Signature: base64.RawURLEncoding.EncodeToString(expiredSig),
	})
	if !expired.IsError {
		t.Fatal("expired challenge was accepted")
	}

	other := connectTestClient(t, ts.socket, "different-client-name")
	if res := callTool(t, other, authproto.ChallengeTool, authproto.ChallengeArgs{GrantID: grant.GrantID}); !res.IsError {
		t.Fatal("grant was accepted for a different client name")
	}
}

func TestDeniedIsStickyAndNoAuthBypasses(t *testing.T) {
	var prompts atomic.Int32
	ts := startTestServer(t, false, func(string, string, time.Duration) (byte, bool) {
		prompts.Add(1)
		return 'n', true
	})
	cs := connectTestClient(t, ts.socket, "denied-client")
	identity, _ := clientauth.New()
	if err := identity.Authorize(context.Background(), cs, ts.core.Sess.ID, "test client"); err == nil {
		t.Fatal("denied client was authorized")
	}
	if err := identity.Authorize(context.Background(), cs, ts.core.Sess.ID, "test client"); err == nil {
		t.Fatal("sticky denial was bypassed")
	}
	if got := prompts.Load(); got != 1 {
		t.Fatalf("denied connection prompted %d times, want 1", got)
	}

	timedOut := startTestServer(t, false, func(string, string, time.Duration) (byte, bool) {
		return 0, false
	})
	timedOutClient := connectTestClient(t, timedOut.socket, "timed-out-client")
	timedOutIdentity, _ := clientauth.New()
	if err := timedOutIdentity.Authorize(context.Background(), timedOutClient, timedOut.core.Sess.ID, "test client"); err == nil {
		t.Fatal("timed-out approval was authorized")
	}

	noAuth := startTestServer(t, true, func(string, string, time.Duration) (byte, bool) {
		t.Fatal("--no-auth prompted")
		return 0, false
	})
	open := connectTestClient(t, noAuth.socket, "open-client")
	if res := callTool(t, open, "set_session_name", map[string]any{"name": "open"}); res.IsError {
		t.Fatalf("--no-auth tool call failed: %#v", res.Content)
	}
}
