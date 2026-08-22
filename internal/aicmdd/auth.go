package aicmdd

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

// defaultApprovalTimeout/defaultChallengeTTL mirror the constants in
// internal/mcpserver/connauth.go; there is no shared source for them since
// this package deliberately doesn't import mcpserver (see plan doc).
const (
	defaultApprovalTimeout = 120 * time.Second
	defaultChallengeTTL    = 30 * time.Second
)

// connAuth is the per-MCP-connection authorization state. Unlike
// internal/mcpserver/connauth.go's version, there is no kernel-verified peer
// (no SO_PEERCRED equivalent for a link relayed from a TCP connection to
// Windows) and no persisted grants (every aishwin.exe restart re-prompts;
// acceptable since these sessions are expected to be long-lived, not
// frequently restarted).
type connAuth struct {
	mu       sync.Mutex
	denied   bool
	grantID  string
	declared string
}

type clientGrant struct {
	publicKey  ed25519.PublicKey
	clientName string
}

type authChallengeState struct {
	grantID string
	nonce   string
	expires time.Time
}

// authManager holds all auth state for one aicmdSession, shared by every MCP
// client connection to its Unix socket.
type authManager struct {
	sess *aicmdSession

	mu         sync.Mutex
	conns      map[*mcp.ServerSession]*connAuth
	grants     map[string]clientGrant
	challenges map[string]authChallengeState
}

func newAuthManager(sess *aicmdSession) *authManager {
	return &authManager{
		sess:       sess,
		conns:      map[*mcp.ServerSession]*connAuth{},
		grants:     map[string]clientGrant{},
		challenges: map[string]authChallengeState{},
	}
}

func (a *authManager) state(ss *mcp.ServerSession) *connAuth {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.conns[ss]
	if st == nil {
		st = &connAuth{}
		a.conns[ss] = st
	}
	return st
}

// middleware rejects every tool call until the connection has obtained an
// interactive grant, mirroring connAuthMiddleware in
// internal/mcpserver/connauth.go. The three auth tools themselves stay
// reachable while gated.
func (a *authManager) middleware() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/call" {
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
			st := a.state(ss)
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

func (a *authManager) requestAccess(ctx context.Context, req *mcp.CallToolRequest, args authproto.RequestAccessArgs) (*mcp.CallToolResult, authproto.RequestAccessResult, error) {
	key, err := decodePublicKey(args.PublicKey)
	if err != nil {
		return nil, authproto.RequestAccessResult{}, err
	}
	st := a.state(req.Session)
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

	name := clientName(req.Session)
	declared := args.ClientDescription
	if declared == "" {
		declared = name
	}
	ans, ok := a.sess.Prompt(fmt.Sprintf("%q wants to control this session — allow?", declared), "yn", defaultApprovalTimeout)
	switch {
	case ok && ans == "y":
	case ok && ans == "n":
		st.denied = true
		return nil, authproto.RequestAccessResult{}, errors.New("the user denied this client access; reconnect to ask again")
	default:
		return nil, authproto.RequestAccessResult{}, errors.New("no response to the authorization prompt; ask the user to approve this client, then retry")
	}

	grantID, err := randomID(16)
	if err != nil {
		return nil, authproto.RequestAccessResult{}, err
	}
	a.mu.Lock()
	if a.conns[req.Session] != st {
		a.mu.Unlock()
		return nil, authproto.RequestAccessResult{}, errors.New("the connection was revoked or closed during approval; reconnect to request again")
	}
	a.grants[grantID] = clientGrant{publicKey: key, clientName: name}
	a.mu.Unlock()
	st.grantID = grantID

	return nil, authproto.RequestAccessResult{GrantID: grantID}, nil
}

func (a *authManager) authChallenge(ctx context.Context, req *mcp.CallToolRequest, args authproto.ChallengeArgs) (*mcp.CallToolResult, authproto.ChallengeResult, error) {
	a.mu.Lock()
	grant, ok := a.grants[args.GrantID]
	a.mu.Unlock()
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
	a.mu.Lock()
	a.pruneChallengesLocked(time.Now())
	a.challenges[challengeID] = authChallengeState{grantID: args.GrantID, nonce: nonce, expires: time.Now().Add(defaultChallengeTTL)}
	a.mu.Unlock()
	return nil, authproto.ChallengeResult{ChallengeID: challengeID, Nonce: nonce, SessionID: a.sess.id}, nil
}

func (a *authManager) authenticate(ctx context.Context, req *mcp.CallToolRequest, args authproto.AuthenticateArgs) (*mcp.CallToolResult, authproto.AuthenticateResult, error) {
	a.mu.Lock()
	challenge, ok := a.challenges[args.ChallengeID]
	delete(a.challenges, args.ChallengeID) // every attempt consumes the challenge
	grant, grantOK := a.grants[args.GrantID]
	a.mu.Unlock()
	if !ok || !grantOK || challenge.grantID != args.GrantID || time.Now().After(challenge.expires) || grant.clientName != clientName(req.Session) {
		return nil, authproto.AuthenticateResult{}, errors.New("invalid or expired authentication challenge")
	}
	sig, err := base64.RawURLEncoding.DecodeString(args.Signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, authproto.AuthenticateResult{}, errors.New("invalid authentication signature")
	}
	payload := authproto.SigningPayload(a.sess.id, args.GrantID, args.ChallengeID, challenge.nonce)
	if !ed25519.Verify(grant.publicKey, payload, sig) {
		return nil, authproto.AuthenticateResult{}, errors.New("invalid authentication signature")
	}
	st := a.state(req.Session)
	st.mu.Lock()
	st.grantID = args.GrantID
	st.mu.Unlock()
	return nil, authproto.AuthenticateResult{Authorized: true}, nil
}

func (a *authManager) pruneChallengesLocked(now time.Time) {
	for id, ch := range a.challenges {
		if now.After(ch.expires) {
			delete(a.challenges, id)
		}
	}
}

func registerAuthTools(s *mcp.Server, a *authManager) {
	mcp.AddTool(s, &mcp.Tool{Name: authproto.RequestAccessTool}, a.requestAccess)
	mcp.AddTool(s, &mcp.Tool{Name: authproto.ChallengeTool}, a.authChallenge)
	mcp.AddTool(s, &mcp.Tool{Name: authproto.AuthenticateTool}, a.authenticate)
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

func randomID(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
