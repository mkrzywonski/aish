package mcpserver

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Connection authorization: a newly connected MCP client cannot drive the
// session until it proves it is operated by someone who can see this
// terminal. On the first gated tool call, aish displays a 6-digit code in
// the terminal (out of band from the shell); the user reads it to the AI,
// which submits it via the authorize tool. Internal same-uid clients
// (cross-session forwarding, the debug CLI) present the session token
// instead, so they never spam the terminal.
//
// The challenge is displayed lazily (first denied call), not on connect, so
// token-authenticating internal clients stay silent.

type connAuth struct {
	code   string
	shown  bool
	authed bool
}

func (c *Core) connState(ss *mcp.ServerSession) *connAuth {
	c.authMu.Lock()
	defer c.authMu.Unlock()
	st := c.conns[ss]
	if st == nil {
		st = &connAuth{code: gen6()}
		c.conns[ss] = st
	}
	return st
}

func (c *Core) forgetConn(ss *mcp.ServerSession) {
	c.authMu.Lock()
	delete(c.conns, ss)
	c.authMu.Unlock()
}

// connAuthMiddleware gates tools/call until the connection is authorized.
// Non-tool methods (initialize, tools/list, notifications) pass through so a
// client can discover the authorize tool; the authorize call itself is never
// gated.
func connAuthMiddleware(c *Core) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/call" {
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
			st := c.connState(ss)
			c.authMu.Lock()
			authed := st.authed
			c.authMu.Unlock()
			if authed {
				return next(ctx, method, req)
			}
			c.presentChallenge(st)
			res := &mcp.CallToolResult{}
			res.SetError(errors.New("this aish session requires authorization: a 6-digit code is displayed in the user's terminal — ask the user to read it to you, then call authorize with it"))
			return res, nil
		}
	}
}

// presentChallenge shows the connection's code in the terminal once.
func (c *Core) presentChallenge(st *connAuth) {
	c.authMu.Lock()
	if st.shown {
		c.authMu.Unlock()
		return
	}
	st.shown = true
	code := st.code
	c.authMu.Unlock()
	c.Sess.Notify("an AI client wants to control this session. To allow it, give it this code: %s", code)
}

// ---- authorize tool ----

type authorizeArgs struct {
	Code string `json:"code" jsonschema:"the 6-digit code shown in the aish terminal (or the internal session token)"`
}

type authorizeResult struct {
	Authorized bool `json:"authorized"`
}

func (c *Core) authorize(ctx context.Context, req *mcp.CallToolRequest, args authorizeArgs) (*mcp.CallToolResult, authorizeResult, error) {
	ss := req.Session
	st := c.connState(ss)
	c.authMu.Lock()
	want := st.code
	c.authMu.Unlock()

	if constEq(args.Code, want) || (c.Token != "" && constEq(args.Code, c.Token)) {
		c.authMu.Lock()
		st.authed = true
		c.authMu.Unlock()
		return nil, authorizeResult{Authorized: true}, nil
	}
	return nil, authorizeResult{}, errors.New("incorrect code")
}

func constEq(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// gen6 returns a random 6-digit code (leading zeros preserved).
func gen6() string {
	var b [8]byte
	rand.Read(b[:])
	n := binary.BigEndian.Uint64(b[:]) % 1000000
	return fmt.Sprintf("%06d", n)
}
