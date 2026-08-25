package aishwnd

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
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
	writeStructured(ctr, m)
}

// writeStructured stores the amended object and keeps the text block in step,
// since the SDK serializes the handler's typed result into both before this
// middleware runs. See annotateSchema for why the schema has to allow the
// field at all.
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

// annotateSchemaMiddleware declares `visibility` in every advertised output
// schema and reopens the schema to extras.
//
// The SDK infers each schema from the handler's typed result and marks it
// additionalProperties:false, then validates the handler's output against it
// INSIDE the typed handler — before this middleware adds anything. So the
// stamped field never faces server-side validation, and a client that checks
// structured content against the advertised schema rejects every call instead.
func annotateSchemaMiddleware() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			if method != "tools/list" || err != nil {
				return res, err
			}
			if lt, ok := res.(*mcp.ListToolsResult); ok && lt != nil {
				schemaMu.Lock()
				for _, t := range lt.Tools {
					patchResultSchema(t)
				}
				schemaMu.Unlock()
			}
			return res, err
		}
	}
}

var schemaMu sync.Mutex

func patchResultSchema(t *mcp.Tool) {
	if t == nil {
		return
	}
	s, ok := t.OutputSchema.(*jsonschema.Schema)
	if !ok || s == nil || s.Type != "object" {
		return
	}
	s.AdditionalProperties = nil
	if s.Properties == nil {
		s.Properties = map[string]*jsonschema.Schema{}
	}
	if _, exists := s.Properties["visibility"]; !exists {
		s.Properties["visibility"] = &jsonschema.Schema{
			Type:        "string",
			Description: "whether the human saw this happen; always \"visible\" on this backend",
		}
	}
}
