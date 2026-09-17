package proxy

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ai-ssh/internal/authproto"
	"ai-ssh/internal/paths"
)

func TestServerInstructionsLeadWithRoutingModel(t *testing.T) {
	if len(serverInstructions) > 2000 {
		t.Fatalf("server instructions are %d bytes; Claude truncates at 2KB", len(serverInstructions))
	}
	lead := serverInstructions
	if len(lead) > 512 {
		lead = lead[:512]
	}
	for _, phrase := range []string{"native shell", "remain local", "use aish tools", "list_sessions", "session_status"} {
		if !strings.Contains(lead, phrase) {
			t.Errorf("critical phrase %q is missing from first 512 bytes", phrase)
		}
	}
}

func TestListDoesNotRemoveUnreachableSocket(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	id := "unreachable"
	if err := os.MkdirAll(paths.SessionDir(id), 0o700); err != nil {
		t.Fatal(err)
	}
	sock := paths.Socket(id)
	if err := os.WriteFile(sock, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := List(); len(got) != 0 {
		t.Fatalf("List returned unreachable session: %#v", got)
	}
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("List removed unreachable socket: %v", err)
	}
}

func TestFilterToolsHidesAuthenticationProtocol(t *testing.T) {
	tools := []*mcp.Tool{{Name: "run_command"}}
	for name := range authproto.InternalTools {
		tools = append(tools, &mcp.Tool{Name: name})
	}
	got := filterTools(tools)
	if len(got) != 1 || got[0].Name != "run_command" {
		t.Fatalf("filterTools = %#v", got)
	}
}

func TestResolveRequiresExplicitTargetWhenAmbiguous(t *testing.T) {
	a := SessionInfo{ID: "abc123", Name: "work"}
	b := SessionInfo{ID: "abc456", Name: "other"}
	p := &aggProxy{}
	for _, tc := range []struct {
		name, target, wantID, wantError string
		live                            []SessionInfo
	}{
		{name: "none", wantError: "no aish sessions"},
		{name: "sole", live: []SessionInfo{a}, wantID: a.ID},
		{name: "multiple", live: []SessionInfo{a, b}, wantError: "several sessions are live"},
		{name: "id", target: a.ID, live: []SessionInfo{a, b}, wantID: a.ID},
		{name: "name", target: b.Name, live: []SessionInfo{a, b}, wantID: b.ID},
		{name: "unique prefix", target: "abc1", live: []SessionInfo{a, b}, wantID: a.ID},
		{name: "ambiguous prefix", target: "abc", live: []SessionInfo{a, b}, wantError: "ambiguous"},
		{name: "duplicate name", target: a.Name, live: []SessionInfo{a, {ID: b.ID, Name: a.Name}}, wantError: "several sessions are named"},
		{name: "missing target", target: "closed", live: []SessionInfo{a}, wantError: "no session matches"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := p.resolve(tc.target, tc.live)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("resolve = %+v, %v; want error containing %q", got, err, tc.wantError)
				}
				return
			}
			if err != nil || got.ID != tc.wantID {
				t.Fatalf("resolve = %+v, %v; want ID %q", got, err, tc.wantID)
			}
		})
	}
}

func TestListSessionsEmptyArray(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	p := &aggProxy{}
	_, result, err := p.listSessions(context.Background(), nil, listSessionsArgs{})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(result)
	if err != nil || string(data) != `{"sessions":[]}` {
		t.Fatalf("empty sessions = %s, %v", data, err)
	}
}

func TestMirroredSessionSchemasFreshAndCached(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	tools := []*mcp.Tool{{
		Name: "file_read",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"session": {Type: "string", Description: "default: the attached session"},
				"path":    {Type: "string"},
			},
			Required: []string{"path"},
		},
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, {Name: "request_access"}}
	before, err := json.Marshal(tools)
	if err != nil {
		t.Fatal(err)
	}
	// Persist the unmodified session schema, as fetching from a live session does.
	saveToolCache(tools)
	fresh, err := mirrorTools(tools)
	if err != nil {
		t.Fatal(err)
	}
	p := &aggProxy{}
	cached, err := p.toolSpecs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for name, mirrored := range map[string][]*mcp.Tool{"fresh": fresh, "cached": cached} {
		t.Run(name, func(t *testing.T) {
			if len(mirrored) != 1 || mirrored[0].Name != "file_read" {
				t.Fatalf("mirrored tools = %+v", mirrored)
			}
			tool := mirrored[0]
			schema := tool.InputSchema.(map[string]any)
			properties := schema["properties"].(map[string]any)
			session := properties["session"].(map[string]any)
			if session["description"] != proxySessionDescription || session["type"] != "string" {
				t.Fatalf("session schema = %+v", session)
			}
			if required := schema["required"].([]any); len(required) != 1 || required[0] != "path" {
				t.Fatalf("required = %v", required)
			}
			if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
				t.Fatal("tool annotations were lost")
			}
		})
	}
	after, _ := json.Marshal(tools)
	if string(before) != string(after) {
		t.Fatal("mirroring mutated the source tool schema")
	}
	loaded, err := os.ReadFile(toolCachePath())
	if err != nil || string(before) != string(loaded) {
		t.Fatal("mirroring rewrote the cached session schema")
	}
}

func TestLegacySessionArgumentWarning(t *testing.T) {
	for _, args := range [][]string{nil, {"--session", "work"}, {"--session=work"}} {
		var output strings.Builder
		warnLegacySessionArgs(&output, args)
		if len(args) == 0 {
			if output.Len() != 0 {
				t.Fatalf("unexpected warning: %s", output.String())
			}
		} else if !strings.Contains(output.String(), "--session is ignored") || !strings.Contains(output.String(), "each tool call's session") {
			t.Fatalf("missing actionable warning: %s", output.String())
		}
	}
}
