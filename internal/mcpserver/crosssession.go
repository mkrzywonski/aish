package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ai-ssh/internal/paths"
	"ai-ssh/internal/proxy"
)

// SessionArg is embedded in every tool's argument struct so each schema
// advertises cross-session targeting. Handlers never read it: dispatch
// happens in the crossSession middleware before the typed layer runs.
type SessionArg struct {
	Session string `json:"session,omitempty" jsonschema:"run this call in another live session, addressed by id or name (see other_sessions in session_status); default: the session this connection is attached to"`
}

// crossSession forwards tools/call requests whose session argument names
// another live session to that session's MCP socket. The forwarded call has
// the argument stripped, so it executes locally at the target (no loops),
// and all of the target session's own guards (echo-off refusal, alt-screen
// errors) apply there.
func crossSession(c *Core) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/call" {
				return next(ctx, method, req)
			}
			params, ok := req.GetParams().(*mcp.CallToolParamsRaw)
			if !ok || len(params.Arguments) == 0 {
				return next(ctx, method, req)
			}
			var args map[string]any
			if err := json.Unmarshal(params.Arguments, &args); err != nil {
				return next(ctx, method, req) // let the tool layer report it
			}
			target, _ := args["session"].(string)
			if target == "" || target == c.Sess.ID || target == paths.ReadName(c.Sess.ID) {
				return next(ctx, method, req)
			}
			delete(args, "session")
			res, err := c.callInSession(ctx, target, params.Name, args)
			if err != nil {
				// Tool-level error, not protocol-level, so the model sees it
				// and can self-correct (e.g. list sessions and retry).
				errRes := &mcp.CallToolResult{}
				errRes.SetError(err)
				return errRes, nil
			}
			return res, nil
		}
	}
}

// callInSession runs one tool call against another session's MCP socket.
func (c *Core) callInSession(ctx context.Context, target, tool string, args map[string]any) (*mcp.CallToolResult, error) {
	info, err := proxy.Resolve(target, proxy.List())
	if err != nil {
		return nil, err
	}
	conn, err := net.Dial("unix", info.Sock)
	if err != nil {
		return nil, fmt.Errorf("session %s: %v", info.Label(), err)
	}
	defer conn.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "aish-cross-session", Version: c.Version}, nil)
	cs, err := client.Connect(ctx, &mcp.IOTransport{Reader: conn, Writer: conn}, nil)
	if err != nil {
		return nil, fmt.Errorf("session %s: %v", info.Label(), err)
	}
	defer cs.Close()
	// Authenticate this internal connection with the target session's token
	// so it isn't blocked by the connection challenge.
	if tok := paths.ReadToken(info.ID); tok != "" {
		if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "authorize", Arguments: map[string]any{"code": tok}}); err != nil {
			return nil, fmt.Errorf("session %s: authorize: %v", info.Label(), err)
		}
	}
	return cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
}
