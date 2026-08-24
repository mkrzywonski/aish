package aishwnd

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// This session kind has no out-of-band route: every command and file operation
// is mirrored to the human's console as it happens. That was stated in one
// tool's prose and nowhere else, so a caller reading any other tool's
// description had to infer visibility from the ABSENCE of a claim — and an
// evaluation agent did exactly that, writing and editing files on a real
// person's machine while unsure whether they would see it.
//
// Saying it in every result costs nothing and removes the inference. The
// shared-terminal server stamps the same field, where the value genuinely
// varies by route; here it is constant, which is itself the useful fact.

const visibilityVisible = "visible"

// visibilityMiddleware stamps `visibility: "visible"` onto every operation's
// result. session_status is exempt: it reports on the session rather than
// doing anything to it, and already answers the question directly through its
// operations_visible field.
func visibilityMiddleware() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			if method != "tools/call" || err != nil {
				return res, err
			}
			params, ok := req.GetParams().(*mcp.CallToolParamsRaw)
			if !ok || params.Name == "session_status" {
				return res, err
			}
			if ctr, ok := res.(*mcp.CallToolResult); ok && ctr != nil {
				annotateVisible(ctr)
			}
			return res, err
		}
	}
}

func annotateVisible(ctr *mcp.CallToolResult) {
	raw, ok := ctr.StructuredContent.(json.RawMessage)
	if !ok {
		return
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	if _, exists := m["visibility"]; exists {
		return
	}
	encoded, err := json.Marshal(visibilityVisible)
	if err != nil {
		return
	}
	m["visibility"] = encoded
	if out, err := json.Marshal(m); err == nil {
		ctr.StructuredContent = json.RawMessage(out)
	}
}
