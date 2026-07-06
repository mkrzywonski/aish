// Package mcpserver exposes a running aish session to MCP clients over a
// per-session Unix socket. Each accepted connection is an independent MCP
// session; tool calls serialize on the session core's locks.
package mcpserver

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ai-ssh/internal/clientauth"
	"ai-ssh/internal/framing"
	"ai-ssh/internal/paths"
	"ai-ssh/internal/session"
	"ai-ssh/internal/sshmux"
	"ai-ssh/internal/state"
	"ai-ssh/internal/term"
)

// Core bundles everything tool handlers need to observe and drive a session.
type Core struct {
	Sess    *session.Session
	Term    *term.Terminal
	Tracker *state.Tracker
	Engine  *framing.Engine
	Mux     *sshmux.Mux
	Tasks   *sshmux.Table
	Version string

	// NoAuth disables the interactive connection-approval prompt (--no-auth):
	// any client that connects can drive the session immediately. For
	// zero-friction sessions where the user doesn't want to approve clients.
	NoAuth bool

	// AutoApprove (--auto-approve) keeps the full authorization handshake in
	// force but auto-answers the approval prompt "yes" (with a Notify, so the
	// approval is still visible) instead of blocking on a human. Unlike NoAuth
	// it doesn't disable the gate — clients still run request_access and prove
	// possession on reconnect — which makes it the realistic path for one-shot
	// testing (e.g. the debug CLI) without an on-disk secret.
	AutoApprove bool

	// oobAlways records a runtime "always" grant of out-of-band access (the
	// 'a' answer). The persistent marker is the OOB file (also written), but
	// this avoids a stat on the hot path once granted in-process.
	oobAlways atomic.Bool

	// authMu guards conns: per-connection authorization state, so a new MCP
	// client must obtain an interactive grant or prove possession of an
	// already-approved client key before its tool calls are honored.
	authMu     sync.Mutex
	conns      map[*mcp.ServerSession]*connAuth
	grants     map[string]clientGrant
	challenges map[string]authChallenge

	// ApprovalPrompt, ApprovalTimeout, and ChallengeTTL are overridable for
	// tests. Production uses Sess.Prompt, 120 seconds, and 30 seconds.
	ApprovalPrompt  func(string, string, time.Duration) (byte, bool)
	ApprovalTimeout time.Duration
	ChallengeTTL    time.Duration

	crossAuthMu sync.Mutex
	crossAuth   *clientauth.Identity

	// OnClients, when set, is called with the number of connected MCP
	// clients whenever it changes (drives the title-bar activity marker).
	OnClients func(n int)

	// OnRenamed, when set, is called after set_session_name persists a new
	// name (drives the title-bar label; the prompt badge re-reads the name
	// file on its own).
	OnRenamed func(name string)
}

// oobGranted reports whether out-of-band (invisible) operations are
// authorized for this session — by --oob, a persisted grant, or an
// in-process "always" answer.
func (c *Core) oobGranted() bool {
	return c.oobAlways.Load() || paths.OOBGranted(c.Sess.ID)
}

// grantOOBAlways records a session-wide out-of-band authorization, both
// in-process and persisted (so the ssh shim honors it for future ssh).
func (c *Core) grantOOBAlways() {
	c.oobAlways.Store(true)
	_ = paths.GrantOOB(c.Sess.ID)
}

// OOBEnabled reports whether out-of-band operations are currently authorized.
// Exported for the Ctrl-] menu (cmd/aish).
func (c *Core) OOBEnabled() bool { return c.oobGranted() }

// SetOOB enables or disables out-of-band authorization at runtime (the Ctrl-]
// menu toggle), updating both the in-process flag and the persisted marker the
// ssh shim reads. Disabling makes route() downgrade to visible in-band ops;
// any idle ControlMaster channel already open is simply no longer used.
func (c *Core) SetOOB(on bool) {
	if on {
		c.grantOOBAlways()
		return
	}
	c.oobAlways.Store(false)
	_ = paths.RevokeOOB(c.Sess.ID)
}

// Serve listens on socketPath until ctx is canceled. It removes any stale
// socket first; callers own directory creation and cleanup.
func Serve(ctx context.Context, core *Core, socketPath string) error {
	_ = os.Remove(socketPath)
	l, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}

	if core.conns == nil {
		core.conns = map[*mcp.ServerSession]*connAuth{}
	}
	if core.grants == nil {
		core.grants = map[string]clientGrant{}
	}
	if core.challenges == nil {
		core.challenges = map[string]authChallenge{}
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "aish", Version: core.Version}, nil)
	registerTools(server, core)
	registerRemoteTools(server, core)
	// Outermost first: gate on connection authorization before anything
	// else (including cross-session forwarding) runs.
	server.AddReceivingMiddleware(connAuthMiddleware(core), crossSession(core))

	go func() {
		<-ctx.Done()
		l.Close()
	}()

	var clients atomic.Int64
	notify := func(delta int64) {
		n := clients.Add(delta)
		if core.OnClients != nil {
			core.OnClients(int(n))
		}
	}

	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go func() {
			ss, err := server.Connect(ctx, &mcp.IOTransport{Reader: conn, Writer: conn}, nil)
			if err != nil {
				conn.Close()
				return
			}
			notify(+1)
			ss.Wait()
			notify(-1)
			core.forgetConn(ss)
		}()
	}
}
