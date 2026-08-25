package mcpserver

import (
	"encoding/json"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestPatchResultSchemaAcceptsStampedFields reproduces the shape the SDK
// actually produces -- an object schema with additionalProperties:false -- and
// checks a result carrying the middleware's fields validates against it.
//
// Without the patch this fails exactly as Amazon Quick did: the command runs,
// the output is present, and the client rejects the result anyway.
func TestPatchResultSchemaAcceptsStampedFields(t *testing.T) {
	closed := &jsonschema.Schema{
		Type:                 "object",
		Properties:           map[string]*jsonschema.Schema{"host": {Type: "string"}},
		AdditionalProperties: &jsonschema.Schema{Not: &jsonschema.Schema{}},
	}
	tool := &mcp.Tool{Name: "file_stat", OutputSchema: closed}

	stamped := []byte(`{"host":"vps","visibility":"silent","target_confidence":"same"}`)

	// Before: the stamped result is rejected.
	resolved, err := closed.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(stamped, &v); err != nil {
		t.Fatal(err)
	}
	if err := resolved.Validate(v); err == nil {
		t.Fatal("expected a closed schema to reject the stamped fields; " +
			"if this passes, the SDK stopped closing inferred schemas and this guard is moot")
	}

	// After: it validates, and both fields are declared rather than merely
	// tolerated.
	patchResultSchema(tool)
	patched := tool.OutputSchema.(*jsonschema.Schema)
	for _, name := range []string{"visibility", "target_confidence"} {
		if _, ok := patched.Properties[name]; !ok {
			t.Errorf("%s is not declared in the patched schema", name)
		}
	}
	resolved, err = patched.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := resolved.Validate(v); err != nil {
		t.Fatalf("patched schema still rejects a stamped result: %v", err)
	}
}

// TestWriteStructuredKeepsTextInStep guards the second half: the SDK builds the
// text block from the pre-middleware JSON, so editing only StructuredContent
// left the two disagreeing and clients reading text never saw the fields.
func TestWriteStructuredKeepsTextInStep(t *testing.T) {
	original := `{"host":"vps"}`
	ctr := &mcp.CallToolResult{
		StructuredContent: json.RawMessage(original),
		Content:           []mcp.Content{&mcp.TextContent{Text: original}},
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(original), &m); err != nil {
		t.Fatal(err)
	}
	m["visibility"] = json.RawMessage(`"silent"`)
	writeStructured(ctr, m)

	text := ctr.Content[0].(*mcp.TextContent).Text
	if text == original {
		t.Fatal("text block still holds the pre-middleware JSON")
	}
	if text != string(ctr.StructuredContent.(json.RawMessage)) {
		t.Fatalf("text and structured content disagree:\n text: %s\n struct: %s",
			text, ctr.StructuredContent)
	}
}

// TestWriteStructuredLeavesHandlerContentAlone: a handler that supplied its own
// content (file_read's text, capture_screen's image) must not have it replaced.
func TestWriteStructuredLeavesHandlerContentAlone(t *testing.T) {
	ctr := &mcp.CallToolResult{
		StructuredContent: json.RawMessage(`{"host":"vps"}`),
		Content:           []mcp.Content{&mcp.TextContent{Text: "file contents, not JSON"}},
	}
	m := map[string]json.RawMessage{"visibility": json.RawMessage(`"silent"`)}
	writeStructured(ctr, m)
	if got := ctr.Content[0].(*mcp.TextContent).Text; got != "file contents, not JSON" {
		t.Fatalf("handler-supplied content was overwritten: %q", got)
	}
}
