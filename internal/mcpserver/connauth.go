package mcpserver

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ai-ssh/internal/authproto"
	"ai-ssh/internal/paths"
)

const (
	defaultApprovalTimeout = 120 * time.Second
	defaultChallengeTTL    = 30 * time.Second
)

type connAuth struct {
	mu      sync.Mutex
	denied  bool
	grantID string   // non-empty once the connection is authorized
	peer    peerInfo // kernel-verified peer creds of this connection
	// declared is the client's self-reported identity from request_access,
	// retained so the Ctrl-] client list can show the same pairing the approval
	// prompt did: what the client claims, beside what the kernel verified.
	declared string
	since    time.Time // accepted at
}

type clientGrant struct {
	publicKey  ed25519.PublicKey
	clientName string
}

type authChallenge struct {
	grantID string
	nonce   string
	expires time.Time
}

func (c *Core) connState(ss *mcp.ServerSession) *connAuth {
	c.authMu.Lock()
	defer c.authMu.Unlock()
	if c.conns == nil {
		c.conns = map[*mcp.ServerSession]*connAuth{}
	}
	st := c.conns[ss]
	if st == nil {
		st = &connAuth{}
		c.conns[ss] = st
	}
	return st
}

func (c *Core) forgetConn(ss *mcp.ServerSession) {
	c.authMu.Lock()
	delete(c.conns, ss)
	c.authMu.Unlock()
}

// setPeer records the kernel-verified peer credentials for a connection,
// captured at accept time and shown (alongside the client's self-declared
// identity) in the approval prompt. It also stamps the connection time, which
// is the only ordering the session has over its clients.
func (c *Core) setPeer(ss *mcp.ServerSession, p peerInfo) {
	st := c.connState(ss)
	st.mu.Lock()
	st.peer = p
	st.since = time.Now()
	st.mu.Unlock()
}

// Revoke clears every client grant and challenge for this session and
// disconnects all currently connected clients, returning the number of live
// connections dropped. Disconnecting (rather than merely deauthorizing) is
// deliberate: a pooled client would otherwise keep reusing its authorized
// connection and never re-run the approval handshake. After a revoke, the next
// access re-requests interactive approval. Under --no-auth this still drops
// connections but reconnects won't prompt.
func (c *Core) Revoke() int {
	c.authMu.Lock()
	c.grants = map[string]clientGrant{}
	c.challenges = map[string]authChallenge{}
	c.clearConfirmedTargets()
	sessions := make([]*mcp.ServerSession, 0, len(c.conns))
	for ss := range c.conns {
		sessions = append(sessions, ss)
	}
	c.conns = map[*mcp.ServerSession]*connAuth{}
	c.authMu.Unlock()
	for _, ss := range sessions {
		_ = ss.Close()
	}
	// Also clear persisted grants so revoked clients must re-approve.
	c.clearPersistedGrants()
	return len(sessions)
}

// connAuthMiddleware rejects every session tool until the connection has
// either obtained an interactive grant or proved possession of an existing
// grant's private key. Authentication tools remain reachable while gated.
func connAuthMiddleware(c *Core) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if c.NoAuth || method != "tools/call" {
				return next(ctx, method, req)
			}
			params, ok := req.GetParams().(*mcp.CallToolParamsRaw)
			if ok && authproto.InternalTools[params.Name] {
				return next(ctx, method, req)
			}
			ss, _ := req.GetSession().(*mcp.ServerSession)
			if ss == nil {
				return authError("client is not authorized; call request_access first"), nil
			}
			st := c.connState(ss)
			st.mu.Lock()
			authed := st.grantID != ""
			st.mu.Unlock()
			if !authed {
				return authError("client is not authorized; call request_access first"), nil
			}
			return next(ctx, method, req)
		}
	}
}

func authError(message string) *mcp.CallToolResult {
	res := &mcp.CallToolResult{}
	res.SetError(errors.New(message))
	return res
}

func (c *Core) requestAccess(ctx context.Context, req *mcp.CallToolRequest, args authproto.RequestAccessArgs) (*mcp.CallToolResult, authproto.RequestAccessResult, error) {
	key, err := decodePublicKey(args.PublicKey)
	if err != nil {
		return nil, authproto.RequestAccessResult{}, err
	}
	st := c.connState(req.Session)
	st.mu.Lock()
	defer st.mu.Unlock()
	if args.ClientDescription != "" {
		st.declared = args.ClientDescription
	}
	if st.denied {
		return nil, authproto.RequestAccessResult{}, errors.New("the user denied this client access; reconnect to ask again")
	}
	if st.grantID != "" {
		return nil, authproto.RequestAccessResult{GrantID: st.grantID}, nil
	}

	// Check if this public key has a persisted grant from a prior approval.
	// This allows PSK-based proxies to reconnect without re-prompting after
	// process restart, as long as the session is still alive.
	if grantID, ok := c.lookupPersistedGrant(key); ok {
		c.authMu.Lock()
		c.grants[grantID] = clientGrant{publicKey: key, clientName: clientName(req.Session)}
		c.authMu.Unlock()
		st.grantID = grantID
		c.Sess.Notify("recognized previously-approved client %s", approvalSubject(args.ClientDescription, st.peer))
		return nil, authproto.RequestAccessResult{GrantID: grantID}, nil
	}

	name := clientName(req.Session)
	declared := args.ClientDescription
	if declared == "" {
		declared = name
	}
	switch {
	case c.NoAuth:
		// gate disabled entirely; nothing to prompt or record
	case c.AutoApprove:
		c.Sess.Notify("auto-approved %s (--auto-approve)", approvalSubject(declared, st.peer))
	default:
		ans, ok := c.prompt(fmt.Sprintf("%s wants to control this session — allow?", approvalSubject(declared, st.peer)))
		switch {
		case ok && ans == 'y':
		case ok && ans == 'n':
			st.denied = true
			return nil, authproto.RequestAccessResult{}, errors.New("the user denied this client access; reconnect to ask again")
		default:
			return nil, authproto.RequestAccessResult{}, errors.New("no response to the authorization prompt; ask the user to approve this client, then retry")
		}
	}

	grantID, err := randomID(16)
	if err != nil {
		return nil, authproto.RequestAccessResult{}, err
	}
	c.authMu.Lock()
	// Revoke() may have run while we were blocked in the prompt (it takes
	// authMu, not st.mu). It resets c.conns, so if our connAuth is no longer the
	// live entry the connection was revoked (and closed) mid-approval — don't
	// resurrect a grant in the freshly-cleared map.
	if c.conns[req.Session] != st {
		c.authMu.Unlock()
		return nil, authproto.RequestAccessResult{}, errors.New("the connection was revoked or closed during approval; reconnect to request again")
	}
	c.grants[grantID] = clientGrant{publicKey: key, clientName: name}
	c.authMu.Unlock()
	st.grantID = grantID

	// Persist the grant so the same public key (PSK-derived or otherwise) can
	// reconnect without prompting after the proxy process restarts.
	c.persistGrant(grantID, key)

	return nil, authproto.RequestAccessResult{GrantID: grantID}, nil
}

func (c *Core) authChallenge(ctx context.Context, req *mcp.CallToolRequest, args authproto.ChallengeArgs) (*mcp.CallToolResult, authproto.ChallengeResult, error) {
	c.authMu.Lock()
	grant, ok := c.grants[args.GrantID]
	c.authMu.Unlock()
	if !ok || grant.clientName != clientName(req.Session) {
		return nil, authproto.ChallengeResult{}, errors.New("unknown client grant")
	}
	challengeID, err := randomID(16)
	if err != nil {
		return nil, authproto.ChallengeResult{}, err
	}
	nonce, err := randomID(32)
	if err != nil {
		return nil, authproto.ChallengeResult{}, err
	}
	c.authMu.Lock()
	c.pruneChallengesLocked(time.Now())
	c.challenges[challengeID] = authChallenge{grantID: args.GrantID, nonce: nonce, expires: time.Now().Add(c.challengeTTL())}
	c.authMu.Unlock()
	return nil, authproto.ChallengeResult{ChallengeID: challengeID, Nonce: nonce, SessionID: c.Sess.ID}, nil
}

func (c *Core) authenticate(ctx context.Context, req *mcp.CallToolRequest, args authproto.AuthenticateArgs) (*mcp.CallToolResult, authproto.AuthenticateResult, error) {
	c.authMu.Lock()
	challenge, ok := c.challenges[args.ChallengeID]
	delete(c.challenges, args.ChallengeID) // every attempt consumes the challenge
	grant, grantOK := c.grants[args.GrantID]
	c.authMu.Unlock()
	if !ok || !grantOK || challenge.grantID != args.GrantID || time.Now().After(challenge.expires) || grant.clientName != clientName(req.Session) {
		return nil, authproto.AuthenticateResult{}, errors.New("invalid or expired authentication challenge")
	}
	sig, err := base64.RawURLEncoding.DecodeString(args.Signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, authproto.AuthenticateResult{}, errors.New("invalid authentication signature")
	}
	payload := authproto.SigningPayload(c.Sess.ID, args.GrantID, args.ChallengeID, challenge.nonce)
	if !ed25519.Verify(grant.publicKey, payload, sig) {
		return nil, authproto.AuthenticateResult{}, errors.New("invalid authentication signature")
	}
	st := c.connState(req.Session)
	st.mu.Lock()
	st.grantID = args.GrantID
	st.mu.Unlock()
	return nil, authproto.AuthenticateResult{Authorized: true}, nil
}

func (c *Core) prompt(question string) (byte, bool) {
	timeout := c.ApprovalTimeout
	if timeout <= 0 {
		timeout = defaultApprovalTimeout
	}
	if c.ApprovalPrompt != nil {
		return c.ApprovalPrompt(question, "yn", timeout)
	}
	return c.Sess.Prompt(question, "yn", timeout)
}

func (c *Core) challengeTTL() time.Duration {
	if c.ChallengeTTL > 0 {
		return c.ChallengeTTL
	}
	return defaultChallengeTTL
}

func (c *Core) pruneChallengesLocked(now time.Time) {
	for id, challenge := range c.challenges {
		if now.After(challenge.expires) {
			delete(c.challenges, id)
		}
	}
}

func clientName(ss *mcp.ServerSession) string {
	if ip := ss.InitializeParams(); ip != nil && ip.ClientInfo != nil && ip.ClientInfo.Name != "" {
		return ip.ClientInfo.Name
	}
	return "an MCP client"
}

// approvalSubject renders who is asking for the approval prompt: the client's
// self-declared identity, followed by the kernel-verified peer process in
// brackets when available. The two are deliberately kept distinct — the first
// is claimed (spoofable), the second is verified — so the user sees both, e.g.
//
//	"Gemini (Antigravity)" [verified: aish (pid 4521, uid 1000)]
func approvalSubject(declared string, peer peerInfo) string {
	subject := fmt.Sprintf("%q", declared)
	if v := peer.String(); v != "" {
		subject += " [verified: " + v + "]"
	}
	return subject
}

func decodePublicKey(encoded string) (ed25519.PublicKey, error) {
	key, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil, errors.New("invalid Ed25519 public key")
	}
	return ed25519.PublicKey(key), nil
}

func randomID(bytes int) (string, error) {
	b := make([]byte, bytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// --- Grant persistence (volatile, tmpfs) ---
//
// Grants are persisted to the session's runtime directory on tmpfs
// (e.g., /run/user/1000/aish/<session-id>/grants.json). This file is:
//   - Volatile: wiped on reboot/logout (tmpfs) and explicitly cleaned by
//     os.RemoveAll(sessionDir) when the session exits.
//   - Scoped: each session has its own grants; a revoked session clears its own file.
//   - Keyed by public key: a PSK-derived proxy always presents the same key,
//     so the session recognizes it without prompting again.

type persistedGrant struct {
	GrantID   string `json:"grant_id"`
	PublicKey string `json:"public_key"` // base64url-encoded
}

func (c *Core) grantsFilePath() string {
	return filepath.Join(paths.SessionDir(c.Sess.ID), "grants.json")
}

func (c *Core) loadPersistedGrants() []persistedGrant {
	b, err := os.ReadFile(c.grantsFilePath())
	if err != nil {
		return nil
	}
	var grants []persistedGrant
	if json.Unmarshal(b, &grants) != nil {
		return nil
	}
	return grants
}

func (c *Core) savePersistedGrants(grants []persistedGrant) {
	b, err := json.Marshal(grants)
	if err != nil {
		return
	}
	_ = os.WriteFile(c.grantsFilePath(), b, 0o600)
}

// persistGrant saves a grant keyed by public key to the session's tmpfs dir.
func (c *Core) persistGrant(grantID string, key ed25519.PublicKey) {
	encoded := base64.RawURLEncoding.EncodeToString(key)
	grants := c.loadPersistedGrants()
	// Replace existing entry for this key, or append.
	found := false
	for i, g := range grants {
		if g.PublicKey == encoded {
			grants[i].GrantID = grantID
			found = true
			break
		}
	}
	if !found {
		grants = append(grants, persistedGrant{GrantID: grantID, PublicKey: encoded})
	}
	c.savePersistedGrants(grants)
}

// lookupPersistedGrant checks if the given public key has been previously
// approved and persisted. Returns the grant ID and true if found.
func (c *Core) lookupPersistedGrant(key ed25519.PublicKey) (string, bool) {
	encoded := base64.RawURLEncoding.EncodeToString(key)
	for _, g := range c.loadPersistedGrants() {
		if g.PublicKey == encoded {
			return g.GrantID, true
		}
	}
	return "", false
}

// clearPersistedGrants removes the grants file (called on Revoke).
func (c *Core) clearPersistedGrants() {
	_ = os.Remove(c.grantsFilePath())
}
