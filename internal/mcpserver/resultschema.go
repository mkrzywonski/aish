package mcpserver

import (
	"context"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// visibility and target_confidence are added to results by middleware, and the
// advertised output schema has to say so or strict clients reject every call.
//
// The SDK generates each tool's outputSchema from the handler's typed result
// struct, and struct inference sets "additionalProperties": false. It then
// validates the handler's output against that schema INSIDE the typed handler
// — which is inside our middleware, so the fields we add afterwards never face
// server-side validation and everything looks fine locally.
//
// A client that validates structured content against the advertised schema
// sees it differently: every result carries two properties the schema forbids,
// so every call fails validation. Observed with Amazon Quick, where each tool
// call surfaced a schema error even though the command had run and its output
// was present. Claude Code does not validate this way, which is exactly why it
// went unnoticed while both fields were designed, reviewed and shipped.
//
// The fix mirrors the middleware that created the problem: one place that
// patches every advertised tool, so a tool added later cannot be forgotten.
// Both halves matter. Declaring the properties documents them for clients that
// read schemas. Clearing additionalProperties keeps the NEXT stamped field
// from breaking those same clients all over again — the failure mode here was
// never one missing property, it was a closed schema meeting an open result.

var routedSchemaMu sync.Mutex

// annotateSchemaMiddleware patches the output schema of every tool the server
// advertises.
func annotateSchemaMiddleware() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			if method != "tools/list" || err != nil {
				return res, err
			}
			if lt, ok := res.(*mcp.ListToolsResult); ok && lt != nil {
				routedSchemaMu.Lock()
				for _, t := range lt.Tools {
					patchResultSchema(t)
				}
				routedSchemaMu.Unlock()
			}
			return res, err
		}
	}
}

// stampedFields are the properties middleware adds to results. Keep this in
// step with visibility.go and targetconfidence.go.
var stampedFields = map[string]*jsonschema.Schema{
	"visibility": {
		Type: "string",
		Description: "whether the human saw this happen in the shared terminal: " +
			"\"visible\", \"silent\" (out of band), or \"unknown\" (route unresolved)",
	},
	"target_confidence": {
		Type: "string",
		Description: "whether aish can confirm this ran on the machine the human is watching: " +
			"\"same\", \"mismatch\", or \"unknown\"",
	},
}

// patchResultSchema declares the stamped fields and reopens the schema.
func patchResultSchema(t *mcp.Tool) {
	if t == nil {
		return
	}
	s, ok := t.OutputSchema.(*jsonschema.Schema)
	if !ok || s == nil || s.Type != "object" {
		return
	}
	// A false additionalProperties is what rejects the stamped fields. Absent
	// means "extras allowed", which is what a result carrying middleware
	// annotations actually is.
	s.AdditionalProperties = nil
	if s.Properties == nil {
		s.Properties = map[string]*jsonschema.Schema{}
	}
	for name, spec := range stampedFields {
		if _, exists := s.Properties[name]; !exists {
			s.Properties[name] = spec
		}
	}
}
