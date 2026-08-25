package mcpserver

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// "Which machine did that just run on" is the question this whole divergence
// feature exists to answer, and until now only two tools answered it.
//
// guardTarget computes the answer for every routed operation, but it returns it
// as a value each handler has to remember to pass on -- and four of the six
// MUTATING call sites dropped it on the floor (`if _, err := c.guardTarget(...)`).
// The tools that kept it were exec and directory_create; the ones that lost it
// were file_write, file_edit, file_patch and file_upload. So the operations most
// able to damage the wrong host were exactly the ones that said nothing about
// which host they had hit, while read-only tools reported it faithfully.
//
// That asymmetry is not a bug in any one handler, it is a bug in making the
// guarantee a per-handler responsibility. So the machine-readable half moves to
// one middleware, for the same reason the activity log and the visibility field
// did: a guarantee that can be forgotten at a call site is worth much less than
// one that cannot, and a mutating tool added later now reports its target
// without its author having to know this feature exists.
//
// The prose note guardTarget returns is still delivered by the handlers that
// have a warning field -- it explains what to DO about a shaky target, which is
// worth saying at length once rather than on every result.
//
// This never opens a channel and never prompts: it reads capability() (the
// non-prompting route) and cached probe facts, the same sources session_status
// uses.

// targetConfidenceMiddleware stamps `target_confidence` onto every tool result
// that describes an operation on a host.
func targetConfidenceMiddleware(c *Core) mcp.Middleware {
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
				c.annotateTargetConfidence(ctr, params.Name)
			}
			return res, err
		}
	}
}

// annotateTargetConfidence adds the field in place, leaving a handler that
// already reported one alone.
func (c *Core) annotateTargetConfidence(ctr *mcp.CallToolResult, tool string) {
	raw, ok := ctr.StructuredContent.(json.RawMessage)
	if !ok {
		return
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	if _, exists := m["target_confidence"]; exists {
		return
	}
	conf := c.targetConfidenceFor(tool, viaField(m))
	if conf == "" {
		return
	}
	encoded, err := json.Marshal(conf)
	if err != nil {
		return
	}
	m["target_confidence"] = encoded
	if out, err := json.Marshal(m); err == nil {
		ctr.StructuredContent = json.RawMessage(out)
	}
}

// targetConfidenceFor answers for one completed call, returning "" when the
// call touched no host and so has no target to be confident about.
func (c *Core) targetConfidenceFor(tool, via string) string {
	switch {
	case controlTools[tool] || skipLogging(tool) || terminalTools[tool]:
		// session_status and probe_host report confidence themselves; the
		// terminal tools act on the shared terminal, which is the human's
		// host by definition.
		return ""
	case via == "":
		// A routed tool that failed before resolving a route. Saying nothing
		// is right: there is no target to describe.
		return ""
	case via == "local" || via == "in_band":
		// Divergence is impossible here, which is why guardTarget returns
		// early for both. A local session is one machine, and an in-band
		// operation types into the very terminal the human is watching.
		return "same"
	case !invisibleRoutes[via]:
		return ""
	}
	// Remaining routes are the out-of-band ones (channel, controlmaster,
	// sftp), where the OOB target and the interactive shell can genuinely be
	// different machines.
	rt := c.capability()
	if rt.via != "controlmaster" && rt.via != "sftp" {
		// The terminal has left the remote this result came from (the human
		// exited ssh while the call was in flight). There is no current
		// interactive host to compare against, so claim nothing.
		return "unknown"
	}
	_, _, _, conf := c.hostConfidence(rt)
	return conf
}
