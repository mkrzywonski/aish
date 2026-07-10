package mcpserver

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ai-ssh/internal/authproto"
)

const (
	defaultApprovalTimeout = 120 * time.Second
	defaultChallengeTTL    = 30 * time.Second
)

type connAuth struct {
	mu      sync.Mutex
	denied  bool
	grantID string // non-empty once the connection is authorized
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
	if st.denied {
		return nil, authproto.RequestAccessResult{}, errors.New("the user denied this client access; reconnect to ask again")
	}
	if st.grantID != "" {
		return nil, authproto.RequestAccessResult{GrantID: st.grantID}, nil
	}

	name := clientName(req.Session)
	switch {
	case c.NoAuth:
		// gate disabled entirely; nothing to prompt or record
	case c.AutoApprove:
		c.Sess.Notify("auto-approved %s (--auto-approve)", name)
	default:
		ans, ok := c.prompt(fmt.Sprintf("%s wants to control this session — allow?", name))
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
