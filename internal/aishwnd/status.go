package aishwnd

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ai-ssh/internal/paths"
)

// session_status is the call every client is told to make after list_sessions.
// Until now this session kind simply did not implement it, so the documented
// opening move failed — and failed with a bare "unknown tool", which reads
// exactly like a typo. Answering it costs nothing: everything here is known
// from the handshake, so the call never reaches the Windows peer and can never
// block, execute, or surprise the human.

type sessionStatusArgs struct{}

type sessionStatusResult struct {
	SessionID   string `json:"session_id"`
	SessionName string `json:"session_name,omitempty"`
	Backend     string `json:"backend"`
	Host        string `json:"host"`
	Platform    string `json:"platform"`
	Proto       int    `json:"wire_proto"`

	AvailableShells []string `json:"available_shells,omitempty"`
	DefaultShell    string   `json:"default_shell,omitempty"`

	// OperationsVisible is true for every tool on this backend. There is
	// no out-of-band route here, so nothing you do is hidden from the human.
	OperationsVisible bool `json:"operations_visible"`

	Note string `json:"note"`
}

const statusNote = "This is a native Windows session reached through aishwnd: there is no shared terminal, " +
	"so terminal state (screen contents, cursor, foreground process, echo_off) does not exist here and the " +
	"terminal tools are not implemented. There is also no out-of-band route, which is why the command tool is " +
	"named run_command rather than exec: everything you run is mirrored to the human's console in real time. " +
	"For the authoritative list of tools this session implements, read the `tools` field for this session in " +
	"list_sessions."

func registerStatusTool(s *mcp.Server, sess *aishwndSession, availableShells []string, defaultShell string, proto int) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "session_status",
		Annotations: readOnlyTool("Windows session status"),
		Description: "Report this Windows session's identity and capabilities: kind, host, platform, the shells " +
			"run_command can use, and whether operations are visible to the human. Answered entirely from the " +
			"connection handshake — it never reaches the Windows host, so it cannot block or execute anything. " +
			"Terminal fields reported by a shared-terminal session (mode, cwd, screen, echo_off, oob_user) do " +
			"do not exist on this backend.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args sessionStatusArgs) (*mcp.CallToolResult, sessionStatusResult, error) {
		return nil, sessionStatusResult{
			SessionID:         sess.id,
			SessionName:       sess.name,
			Backend:           paths.BackendDirectHost,
			Host:              sess.displayHost(),
			Platform:          "windows",
			Proto:             proto,
			AvailableShells:   availableShells,
			DefaultShell:      defaultShell,
			OperationsVisible: true,
			Note:              statusNote,
		}, nil
	})
}
