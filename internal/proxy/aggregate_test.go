package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// schema builds a mirrored (JSON-decoded) input schema with the given property
// names, matching the shape ListTools hands back over the wire.
func schema(props ...string) any {
	p := map[string]any{}
	for _, name := range props {
		p[name] = map[string]any{"type": "string"}
	}
	return map[string]any{
		"type":                 "object",
		"properties":           p,
		"additionalProperties": false,
	}
}

func names(tools []*mcp.Tool) []string {
	out := make([]string, len(tools))
	for i, t := range tools {
		out[i] = t.Name
	}
	return out
}

func find(t *testing.T, tools []*mcp.Tool, name string) *mcp.Tool {
	t.Helper()
	for _, tool := range tools {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("tool %q not advertised; got %v", name, names(tools))
	return nil
}

// The whole point of merging: a tool only one backend implements must
// still be advertised, or it has no handler registered and is unroutable even
// though its session serves it.
func TestMergeToolSpecsUnionsAcrossSessions(t *testing.T) {
	merged := mergeToolSpecs([]labeledTools{
		{label: "aaa (nixos)", backend: "shared_terminal", tools: []*mcp.Tool{
			{Name: "run_command", Description: "type into the shared terminal", InputSchema: schema("command", "session")},
			{Name: "file_read", Description: "read a file", InputSchema: schema("path", "session")},
		}},
		{label: "bbb (Windows)", backend: "direct_host", tools: []*mcp.Tool{
			{Name: "capture_screen", Description: "screenshot the host", InputSchema: schema("mode")},
			{Name: "file_read", Description: "read a file", InputSchema: schema("path", "session")},
		}},
	})
	for _, want := range []string{"run_command", "capture_screen", "file_read"} {
		find(t, merged, want)
	}
	if len(merged) != 3 {
		t.Errorf("got %d tools (%v), want 3 with file_read merged once", len(merged), names(merged))
	}
}

// A session server that serves one session omits `session` and sets
// additionalProperties:false, which together forbid the only argument the
// proxy needs to route the call.
func TestMergeToolSpecsInjectsSessionArg(t *testing.T) {
	merged := mergeToolSpecs([]labeledTools{
		{label: "bbb (Windows)", backend: "direct_host", tools: []*mcp.Tool{
			{Name: "capture_screen", Description: "screenshot the host", InputSchema: schema("mode")},
		}},
	})
	props, ok := schemaProperties(find(t, merged, "capture_screen").InputSchema)
	if !ok {
		t.Fatal("merged schema has no properties map")
	}
	if _, exists := props["session"]; !exists {
		t.Fatal("session argument was not injected into a schema that lacked it")
	}
}

func TestEnsureSessionArgLeavesAnExistingOneAlone(t *testing.T) {
	tool := &mcp.Tool{Name: "exec", InputSchema: schema("command", "session")}
	props, _ := schemaProperties(tool.InputSchema)
	props["session"] = map[string]any{"type": "string", "description": "original wording"}
	ensureSessionArg(tool)
	got, _ := schemaProperties(tool.InputSchema)
	sessionProp, _ := got["session"].(map[string]any)
	if sessionProp["description"] != "original wording" {
		t.Errorf("existing session property was overwritten: %v", sessionProp)
	}
}

// Same name, different contract: the advertised description must say so rather
// than presenting one backend's variant as universal.
func TestMergeToolSpecsFlagsDivergentVariants(t *testing.T) {
	merged := mergeToolSpecs([]labeledTools{
		{label: "aaa (nixos)", backend: "shared_terminal", tools: []*mcp.Tool{
			{Name: "exec", Description: "invisible out-of-band execution", InputSchema: schema("command", "session")},
		}},
		{label: "bbb (Windows)", backend: "direct_host", tools: []*mcp.Tool{
			{Name: "exec", Description: "visible, mirrored to the human's console", InputSchema: schema("command", "shell")},
		}},
	})
	desc := find(t, merged, "exec").Description
	if !strings.Contains(desc, "different variants") {
		t.Errorf("divergent tool advertised without a warning: %q", desc)
	}
	if !strings.Contains(desc, "direct_host") {
		t.Errorf("warning does not name the diverging backend: %q", desc)
	}
	// Session ids change on every restart; a note pointing at one goes stale.
	if strings.Contains(desc, "bbb (Windows)") {
		t.Errorf("warning names a session instead of a backend: %q", desc)
	}
	// The routing-aware variant is the one that can actually be addressed.
	if !strings.Contains(desc, "invisible out-of-band") {
		t.Errorf("expected the session-declaring variant as the base: %q", desc)
	}
}

func TestMergeToolSpecsQuietWhenVariantsAgree(t *testing.T) {
	identical := func() *mcp.Tool {
		return &mcp.Tool{Name: "file_stat", Description: "stat a path", InputSchema: schema("path", "session")}
	}
	merged := mergeToolSpecs([]labeledTools{
		{label: "aaa (nixos)", backend: "shared_terminal", tools: []*mcp.Tool{identical()}},
		{label: "bbb (Windows)", backend: "direct_host", tools: []*mcp.Tool{identical()}},
	})
	if desc := find(t, merged, "file_stat").Description; desc != "stat a path" {
		t.Errorf("identical variants should not be annotated, got %q", desc)
	}
}

// Dropping the pooled connection on a tool-level refusal forced a reconnect
// and a full re-authorization on the next call. Only a dead link justifies it.
func TestIsTransportError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unknown tool", errors.New(`calling "tools/call": unknown tool "session_status"`), false},
		{"bad argument", errors.New("path must not be empty"), false},
		{"eof", io.EOF, true},
		{"unexpected eof", io.ErrUnexpectedEOF, true},
		{"closed", net.ErrClosed, true},
		{"wrapped closed", errors.Join(errors.New("write"), net.ErrClosed), true},
	} {
		if got := isTransportError(tc.err); got != tc.want {
			t.Errorf("%s: isTransportError(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}

// A bare `unknown tool "x"` was worded identically to a typo and named neither
// the reason nor the alternatives.
func TestUnsupportedToolErrorIsActionable(t *testing.T) {
	p := &aggProxy{
		conns:     map[string]*pooledConn{},
		lastNames: map[string]string{},
		toolNames: map[string][]string{
			"88f98135": {"directory_list", "file_read", "run_command"},
		},
	}
	info := SessionInfo{ID: "88f98135", Name: "Windows", Backend: "direct_host"}
	msg := p.unsupportedToolError(context.Background(), info, "session_status")
	if msg == "" {
		t.Fatal("calling a tool the session does not implement was allowed through")
	}
	for _, want := range []string{"session_status", "Windows", "direct_host", "not a typo", "run_command"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal should mention %q; got: %s", want, msg)
		}
	}
}

func TestUnsupportedToolErrorAllowsImplementedTools(t *testing.T) {
	p := &aggProxy{
		conns:     map[string]*pooledConn{},
		lastNames: map[string]string{},
		toolNames: map[string][]string{"88f98135": {"file_read", "run_command"}},
	}
	info := SessionInfo{ID: "88f98135", Name: "Windows", Backend: "direct_host"}
	if msg := p.unsupportedToolError(context.Background(), info, "file_read"); msg != "" {
		t.Errorf("implemented tool was refused: %s", msg)
	}
}

// An unknown surface must not become a tollgate: if we could not learn what a
// session implements, the session still gets to answer for itself.
func TestUnsupportedToolErrorSilentWhenSurfaceUnknown(t *testing.T) {
	p := &aggProxy{conns: map[string]*pooledConn{}, lastNames: map[string]string{}, toolNames: map[string][]string{}}
	info := SessionInfo{ID: "nosuch", Sock: "/nonexistent/aish.sock"}
	if msg := p.unsupportedToolError(context.Background(), info, "anything"); msg != "" {
		t.Errorf("should defer to the session when its tools are unknown, got: %s", msg)
	}
}

// Advertising one variant's schema silently removed the other's arguments from
// the client's reach: a direct-host run_command accepts background/shell/cwd,
// none of which survived, so a background command could not be started at all.
func TestMergeToolSpecsUnionsArguments(t *testing.T) {
	merged := mergeToolSpecs([]labeledTools{
		{label: "aaa (nixos)", backend: "shared_terminal", tools: []*mcp.Tool{
			{Name: "run_command", Description: "type into the terminal", InputSchema: schema("command", "timeout_ms", "session")},
		}},
		{label: "bbb (Windows)", backend: "direct_host", tools: []*mcp.Tool{
			{Name: "run_command", Description: "mirrored to the console", InputSchema: schema("command", "timeout_ms", "background", "shell", "cwd")},
		}},
	})
	props, ok := schemaProperties(find(t, merged, "run_command").InputSchema)
	if !ok {
		t.Fatal("merged schema lost its properties")
	}
	for _, want := range []string{"command", "timeout_ms", "session", "background", "shell", "cwd"} {
		if _, exists := props[want]; !exists {
			t.Errorf("argument %q is not reachable through the advertised schema", want)
		}
	}
	// A backend-specific argument must say so, or a caller cannot tell why it
	// was rejected on the other backend.
	spec, _ := props["background"].(map[string]any)
	desc, _ := spec["description"].(string)
	if !strings.Contains(desc, "direct_host") {
		t.Errorf("backend-specific argument is not labelled: %q", desc)
	}
	// An argument both accept must NOT be labelled.
	cspec, _ := props["command"].(map[string]any)
	cdesc, _ := cspec["description"].(string)
	if strings.Contains(cdesc, "accepted only by") {
		t.Errorf("shared argument was labelled backend-specific: %q", cdesc)
	}
}
