package proxy

import (
	"os"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ai-ssh/internal/authproto"
	"ai-ssh/internal/paths"
)

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
