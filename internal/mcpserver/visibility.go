package mcpserver

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Whether an operation was visible to the human is the single most important
// fact about it on this system: the escalation rules, the consent gate and the
// activity log all hang off that distinction. Yet a caller could only work it
// out AFTER the fact, by reading `via` and knowing which routes are invisible,
// by diffing the screen, or by consulting oob_log. Two independent evaluation
// agents, working on different session kinds with no knowledge of each other,
// each asked for the same missing thing — one of them having already created
// and edited a file on someone's machine before wondering whether they'd seen
// it happen.
//
// So every tool result now says so outright. This is done as one middleware
// rather than a field set by each handler for the same reason the activity log
// is: a guarantee that can be forgotten at a call site is worth much less than
// one that cannot. Classification reuses the log's own route tables, so the
// answer a caller reads and the answer the audit trail records cannot drift
// apart.

const (
	visibilityVisible = "visible" // the human saw this happen in their terminal
	visibilitySilent  = "silent"  // this happened out of band; nothing appeared on screen
	visibilityUnknown = "unknown" // route unresolved — claim nothing
)

// visibilityMiddleware stamps `visibility` onto every tool result that
// describes an operation. It runs after the handler, so it annotates the same
// structured result the caller receives.
func visibilityMiddleware() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			if method != "tools/call" || err != nil {
				return res, err
			}
			params, ok := req.GetParams().(*mcp.CallToolParamsRaw)
			if !ok {
				return res, err
			}
			if ctr, ok := res.(*mcp.CallToolResult); ok && ctr != nil {
				annotateVisibility(ctr, params.Name)
			}
			return res, err
		}
	}
}

// annotateVisibility adds the field in place, leaving a handler that already
// set one alone.
func annotateVisibility(ctr *mcp.CallToolResult, tool string) {
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
	v := visibilityOf(tool, viaField(m))
	if v == "" {
		return
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		return
	}
	m["visibility"] = encoded
	writeStructured(ctr, m)
}

// writeStructured stores the amended object and keeps the text block in step
// with it.
//
// The SDK serializes the handler's typed result into BOTH StructuredContent
// and a TextContent block, and it does that before our middleware runs. Editing
// only the structured half left the two disagreeing: a client that reads the
// text block — many do, and it is what the spec suggests for exactly this
// purpose — would never see the fields we stamped, silently losing the
// annotation for that whole class of client. So the text block is rewritten
// too, but only when it still holds precisely what the SDK put there; a handler
// that supplied its own content is left alone.
func writeStructured(ctr *mcp.CallToolResult, m map[string]json.RawMessage) {
	before, _ := ctr.StructuredContent.(json.RawMessage)
	out, err := json.Marshal(m)
	if err != nil {
		return
	}
	ctr.StructuredContent = json.RawMessage(out)
	if len(ctr.Content) != 1 {
		return
	}
	if tc, ok := ctr.Content[0].(*mcp.TextContent); ok && tc.Text == string(before) {
		tc.Text = string(out)
	}
}

// viaField reads the route a result reported, or "" when it reported none.
func viaField(m map[string]json.RawMessage) string {
	raw, ok := m["via"]
	if !ok {
		return ""
	}
	var via string
	if json.Unmarshal(raw, &via) != nil {
		return ""
	}
	return via
}

// visibilityOf classifies one completed call, returning "" for calls that
// perform no operation at all and so have no visibility to report.
//
// The unresolved case says "unknown" rather than guessing. A routed tool that
// fails before its route resolves reports no via, and answering "visible"
// there would be the one wrong answer that matters: it would tell a caller the
// human had seen something they did not.
func visibilityOf(tool, via string) string {
	switch {
	case controlTools[tool] || skipLogging(tool):
		// Nothing happened that could have been seen or hidden: these report
		// a route without taking it (session_status), read aish's own state
		// (oob_log), or are protocol calls. Answering "unknown" for them
		// implied we could not tell whether the human saw something, when in
		// fact there was nothing to see.
		return ""
	case terminalTools[tool]:
		return visibilityVisible
	case via == "":
		return visibilityUnknown
	case invisibleRoutes[via]:
		return visibilitySilent
	default:
		return visibilityVisible
	}
}
