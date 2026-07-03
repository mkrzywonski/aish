package mcpserver

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Connection authorization: a newly connected MCP client cannot drive the
// session until the user approves it with a y/n prompt in the terminal (out
// of band from the shell — see session/console.go). This is a consent and
// awareness control, not a defense against same-uid code: the socket lives
// in a 0700 dir, so only same-uid processes can connect at all, and a
// hostile same-uid process is out of scope (it can already read your files
// and scrape your tty). What this guarantees is that YOU are aware of, and
// approved, every client that takes control of the session.
//
// Internal same-uid helpers (cross-session forwarding, the debug CLI) present
// the per-session token via the authorize tool and are never prompted.
//
// --no-auth (Core.NoAuth) disables the prompt for a zero-friction session.

type connAuth struct {
	mu     sync.Mutex
	authed bool
	denied bool // explicit "no"; sticky so a client can't re-prompt-spam you
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

// connAuthMiddleware gates tools/call until the user approves the connection.
// Non-tool methods (initialize, tools/list, notifications) pass through so a
// client can complete its handshake; the authorize call (internal token
// path) is never gated.
func connAuthMiddleware(c *Core) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if c.NoAuth || method != "tools/call" {
				return next(ctx, method, req)
			}
			params, ok := req.GetParams().(*mcp.CallToolParamsRaw)
			if ok && params.Name == "authorize" {
				return next(ctx, method, req)
			}
			ss, _ := req.GetSession().(*mcp.ServerSession)
			if ss == nil {
				return next(ctx, method, req)
			}
			if err := c.ensureApproved(ss); err != nil {
				res := &mcp.CallToolResult{}
				res.SetError(err)
				return res, nil
			}
			return next(ctx, method, req)
		}
	}
}

// ensureApproved returns nil once the connection is approved, prompting the
// user y/n on first use. The per-connection lock collapses concurrent first
// calls into a single prompt.
func (c *Core) ensureApproved(ss *mcp.ServerSession) error {
	st := c.connState(ss)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.authed {
		return nil
	}
	if st.denied {
		return errors.New("the user denied this client access to the session; reconnect to ask again")
	}
	ans, ok := c.Sess.Prompt(fmt.Sprintf("%s wants to control this session — allow?", clientName(ss)), "yn", 120*time.Second)
	switch {
	case ok && ans == 'y':
		st.authed = true
		return nil
	case ok && ans == 'n':
		st.denied = true
		return errors.New("the user denied this client access to the session; reconnect to ask again")
	default:
		return errors.New("no response to the authorization prompt; ask the user to approve this client in their terminal, then retry")
	}
}

func clientName(ss *mcp.ServerSession) string {
	if ip := ss.InitializeParams(); ip != nil && ip.ClientInfo != nil && ip.ClientInfo.Name != "" {
		return "an AI client (" + ip.ClientInfo.Name + ")"
	}
	return "an AI client"
}

// ---- authorize tool (internal token path) ----

type authorizeArgs struct {
	Token string `json:"token" jsonschema:"the internal per-session token (used by same-uid helpers to bypass the connection prompt)"`
}

type authorizeResult struct {
	Authorized bool `json:"authorized"`
}

func (c *Core) authorize(ctx context.Context, req *mcp.CallToolRequest, args authorizeArgs) (*mcp.CallToolResult, authorizeResult, error) {
	if c.Token == "" || !constEq(args.Token, c.Token) {
		return nil, authorizeResult{}, errors.New("invalid token")
	}
	st := c.connState(req.Session)
	st.mu.Lock()
	st.authed = true
	st.denied = false
	st.mu.Unlock()
	return nil, authorizeResult{Authorized: true}, nil
}

func constEq(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
