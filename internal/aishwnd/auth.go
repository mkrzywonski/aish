package aishwnd

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
	"sort"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ai-ssh/internal/authproto"
	"ai-ssh/internal/paths"
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
// (no SO_PEERCRED equivalent for a link relayed from a TCP connection to Windows).
// Grants ARE persisted to the session directory so that PSK-based proxies
// (which always present the same public key) can reconnect without re-prompting
// after the proxy process restarts, as long as the aishwnd session is alive.
type connAuth struct {
	mu       sync.Mutex
	denied   bool
	grantID  string
	declared string
	// connID identifies this connection for the Windows console's Clients
	// dialog (list_clients/disconnect_client) -- a *mcp.ServerSession
	// pointer isn't something the wire protocol can name, so this is the
	// stable handle sent instead. since is stamped once, the first time
	// state() creates this entry (the connection-accepted moment, there
	// being no separate kernel-verified-peer step to hang it off of the
	// way internal/mcpserver/connauth.go's setPeer does).
	connID string
	since  time.Time
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

// authManager holds all auth state for one aishwndSession, shared by every MCP
// client connection to its Unix socket.
type authManager struct {
	sess *aishwndSession

	mu         sync.Mutex
	conns      map[*mcp.ServerSession]*connAuth
	grants     map[string]clientGrant
	challenges map[string]authChallengeState
}

func newAuthManager(sess *aishwndSession) *authManager {
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
		connID, err := randomID(8)
		if err != nil {
			connID = "" // extremely unlikely; still a usable map entry, just not nameable by the Clients dialog
		}
		st = &connAuth{connID: connID, since: time.Now()}
		a.conns[ss] = st
	}
	return st
}

// Connection states reported to the Windows console's Clients dialog,
// mirroring internal/mcpserver/clients.go's constants (aishwnd can't
// import that package, and has no NoAuth/AutoApprove equivalent to
// account for -- cmd/aishwnd has no flags for either).
const (
	clientAuthorized = "authorized"
	clientPending    = "awaiting approval"
	clientDenied     = "denied"
)

// clientInfo is one connected MCP client as this session sees it, the
// aishwnd-side counterpart to internal/mcpserver/clients.go's
// ConnectedClient.
type clientInfo struct {
	ID          string
	Name        string
	Version     string
	Description string
	State       string
	Since       time.Time
}

// listClients returns a snapshot of the live MCP connections to this
// session's Unix socket, oldest first -- mirroring
// internal/mcpserver/clients.go's ConnectedClients, adapted for the
// simpler state this package tracks.
func (a *authManager) listClients() []clientInfo {
	a.mu.Lock()
	sessions := make([]*mcp.ServerSession, 0, len(a.conns))
	states := make([]*connAuth, 0, len(a.conns))
	for ss, st := range a.conns {
		sessions = append(sessions, ss)
		states = append(states, st)
	}
	a.mu.Unlock()

	out := make([]clientInfo, 0, len(states))
	for i, st := range states {
		st.mu.Lock()
		info := clientInfo{ID: st.connID, Description: st.declared, Since: st.since}
		switch {
		case st.grantID != "":
			info.State = clientAuthorized
		case st.denied:
			info.State = clientDenied
		default:
			info.State = clientPending
		}
		st.mu.Unlock()

		if ss := sessions[i]; ss != nil {
			if ip := ss.InitializeParams(); ip != nil && ip.ClientInfo != nil {
				info.Name = ip.ClientInfo.Name
				info.Version = ip.ClientInfo.Version
			}
		}
		if info.Name == "" {
			info.Name = "an MCP client"
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out
}

// disconnectClient closes one specific connection (identified by the ID
// listClients reported) and forgets its grant, so it can't silently
// reconnect and keep using it without a fresh approval prompt. Reports
// whether a matching connection was found; the caller (handleDisconnectClient)
// turns that into a wire error.
func (a *authManager) disconnectClient(id string) bool {
	if id == "" {
		return false
	}
	a.mu.Lock()
	var target *mcp.ServerSession
	var targetState *connAuth
	for ss, st := range a.conns {
		st.mu.Lock()
		match := st.connID == id
		st.mu.Unlock()
		if match {
			target = ss
			targetState = st
			break
		}
	}
	if target == nil {
		a.mu.Unlock()
		return false
	}
	delete(a.conns, target)
	targetState.mu.Lock()
	grantID := targetState.grantID
	targetState.mu.Unlock()
	if grantID != "" {
		delete(a.grants, grantID)
	}
	a.mu.Unlock()

	_ = target.Close()
	return true
}

// forget removes ss's tracked connection state once its connection has
// actually closed (for any reason) -- called from serveUnix after
// ss.Wait() returns. Unlike disconnectClient, this never touches
// a.grants: the whole point of a grant is to survive exactly this kind of
// ordinary reconnect (the aish proxy's own connection pool can
// legitimately close and reopen a connection) without re-prompting, so
// only the stale per-connection state that made this connection LOOK
// still-open goes away here. Without this, a.conns only ever shrank via
// an explicit Disconnect click, so listClients kept reporting every
// connection that had EVER authenticated, not just what's still live --
// found live testing the status bar's client-count indicator, which
// polls listClients periodically and would otherwise only ever grow.
func (a *authManager) forget(ss *mcp.ServerSession) {
	a.mu.Lock()
	delete(a.conns, ss)
	a.mu.Unlock()
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

	// Check if this public key has a persisted grant from a prior approval.
	// This allows PSK-based proxies to reconnect without re-prompting after
	// process restart, as long as the session is still alive.
	if grantID, ok := a.lookupPersistedGrant(key); ok {
		a.mu.Lock()
		a.grants[grantID] = clientGrant{publicKey: key, clientName: clientName(req.Session)}
		a.mu.Unlock()
		st.grantID = grantID
		recognized := args.ClientDescription
		if recognized == "" {
			recognized = clientName(req.Session)
		}
		a.sess.Notify("recognized previously-approved client %q", recognized)
		return nil, authproto.RequestAccessResult{GrantID: grantID}, nil
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

	// Persist the grant so the same public key (PSK-derived or otherwise) can
	// reconnect without prompting after the proxy process restarts.
	a.persistGrant(grantID, key)

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

// --- Grant persistence ---
//
// Mirrors internal/mcpserver/connauth.go's grant persistence: writes approved
// grants to a JSON file in the session directory, keyed by public key. This
// allows a PSK-based proxy (which always presents the same public key) to
// reconnect without re-prompting after its process restarts. The grants file
// lives in the session dir and is removed when the session exits (the session
// dir is cleaned up by os.RemoveAll in session.go's Run), so grants are scoped
// to one aishwnd session lifetime — matching the regular aish behavior.

type persistedGrant struct {
	GrantID   string `json:"grant_id"`
	PublicKey string `json:"public_key"` // base64url-encoded
}

func (a *authManager) grantsFilePath() string {
	return filepath.Join(paths.SessionDir(a.sess.id), "grants.json")
}

func (a *authManager) loadPersistedGrants() []persistedGrant {
	b, err := os.ReadFile(a.grantsFilePath())
	if err != nil {
		return nil
	}
	var grants []persistedGrant
	if json.Unmarshal(b, &grants) != nil {
		return nil
	}
	return grants
}

func (a *authManager) savePersistedGrants(grants []persistedGrant) {
	b, err := json.Marshal(grants)
	if err != nil {
		return
	}
	_ = os.WriteFile(a.grantsFilePath(), b, 0o600)
}

func (a *authManager) persistGrant(grantID string, key ed25519.PublicKey) {
	encoded := base64.RawURLEncoding.EncodeToString(key)
	grants := a.loadPersistedGrants()
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
	a.savePersistedGrants(grants)
}

// lookupPersistedGrant checks if the given public key has been previously
// approved and persisted. Returns the grant ID and true if found.
func (a *authManager) lookupPersistedGrant(key ed25519.PublicKey) (string, bool) {
	encoded := base64.RawURLEncoding.EncodeToString(key)
	for _, g := range a.loadPersistedGrants() {
		if g.PublicKey == encoded {
			return g.GrantID, true
		}
	}
	return "", false
}

func randomID(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
