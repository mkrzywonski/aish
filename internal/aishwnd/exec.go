package aishwnd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ai-ssh/internal/aishwinwire"
)

// execArgs/execResult and execStatusArgs/execStatusResult mirror aish's own
// exec/exec_status schemas (internal/mcpserver/tools_remote.go's execArgs
// etc.) minus the SessionArg routing field — aishwnd doesn't implement
// cross-session forwarding, so declaring a field for it would advertise a
// capability that doesn't exist.
type execArgs struct {
	Command    string `json:"command"`
	Cwd        string `json:"cwd,omitempty" jsonschema:"absolute working directory on the Windows host"`
	Background bool   `json:"background,omitempty"`
	TimeoutMs  int    `json:"timeout_ms,omitempty" jsonschema:"foreground only; default 30000"`
	Shell      string `json:"shell,omitempty" jsonschema:"which persistent shell runs this command -- see the tool description for what's available on this host and the default when omitted"`
}

type execResult struct {
	Output   string `json:"output,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
	TaskID   string `json:"task_id,omitempty"`
	TimedOut bool   `json:"timed_out,omitempty"`
	Shell    string `json:"shell"`
	Via      string `json:"via"`
	Host     string `json:"host"`
}

type execStatusArgs struct {
	TaskID string `json:"task_id"`
	Cursor *int64 `json:"cursor,omitempty" jsonschema:"pass next_cursor from the previous poll"`
}

type execStatusResult struct {
	Running    bool   `json:"running"`
	Output     string `json:"output"`
	NextCursor int64  `json:"next_cursor"`
	ExitCode   *int   `json:"exit_code,omitempty"`
}

func registerExecTools(s *mcp.Server, sess *aishwndSession, availableShells []string, defaultShell string) {
	if defaultShell == "" {
		defaultShell = "powershell"
	}
	if len(availableShells) == 0 {
		availableShells = []string{defaultShell}
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "exec",
		Annotations: commandTool("Run command on Windows host"),
		Description: "Run a command on the Windows host, visibly — it and its output are mirrored to the human's " +
			"console in real time, the same way run_command works for a shared PTY session; there's no shared " +
			"terminal here to be invisible relative to. Runs against a persistent shell that keeps its working " +
			"directory and environment between calls; set cwd to change directory just for this one command. " +
			"Available shells on this host: " + strings.Join(availableShells, ", ") + " (default " + defaultShell +
			" when shell is omitted; powershell is the more capable option for most tasks, cmd for simpler " +
			"cases). Each shell kind keeps its OWN persistent process with its own working directory and " +
			"environment, independent of the others — " +
			"switching which one you use for one call never loses another kind's state. " +
			"Use background=true for long-running commands, then poll exec_status. A foreground command that " +
			"times out kills and replaces that shell's persistent process (state is lost) rather than risk its " +
			"late output corrupting the next command's result.",
	}, sess.execTool)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "exec_status",
		Description: "Poll a background task started by exec: incremental output (pass next_cursor back), running state, exit code.",
		Annotations: readOnlyTool("Poll background command"),
	}, sess.execStatus)
}

func (s *aishwndSession) execTool(ctx context.Context, req *mcp.CallToolRequest, args execArgs) (*mcp.CallToolResult, execResult, error) {
	if args.Command == "" {
		return nil, execResult{}, errors.New("command must not be empty")
	}

	data, err := json.Marshal(aishwinwire.ExecData{
		Command:    args.Command,
		Cwd:        args.Cwd,
		Background: args.Background,
		TimeoutMs:  args.TimeoutMs,
		Shell:      args.Shell,
	})
	if err != nil {
		return nil, execResult{}, err
	}

	raw, err := s.roundTrip("exec", data, execWaitTimeout(args))
	if err != nil {
		return nil, execResult{}, err
	}
	var res aishwinwire.ExecResultData
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, execResult{}, fmt.Errorf("malformed exec result from the Windows peer: %w", err)
	}
	if res.Error != "" {
		return nil, execResult{}, errors.New(res.Error)
	}
	return nil, execResult{
		Output:   res.Output,
		ExitCode: res.ExitCode,
		TaskID:   res.TaskID,
		TimedOut: res.TimedOut,
		Shell:    res.Shell,
		Via:      "aishwin",
		Host:     s.displayHost(),
	}, nil
}

func (s *aishwndSession) execStatus(ctx context.Context, req *mcp.CallToolRequest, args execStatusArgs) (*mcp.CallToolResult, execStatusResult, error) {
	if args.TaskID == "" {
		return nil, execStatusResult{}, errors.New("task_id must not be empty")
	}
	cursor := int64(0)
	if args.Cursor != nil {
		cursor = *args.Cursor
	}
	data, err := json.Marshal(aishwinwire.ExecPollData{TaskID: args.TaskID, Cursor: cursor})
	if err != nil {
		return nil, execStatusResult{}, err
	}

	raw, err := s.roundTrip("exec_poll", data, 10*time.Second)
	if err != nil {
		return nil, execStatusResult{}, err
	}
	var res aishwinwire.ExecPollResultData
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, execStatusResult{}, fmt.Errorf("malformed exec_poll result from the Windows peer: %w", err)
	}
	if res.Error != "" {
		return nil, execStatusResult{}, errors.New(res.Error)
	}
	return nil, execStatusResult{
		Running:    res.Running,
		Output:     res.Output,
		NextCursor: res.NextCursor,
		ExitCode:   res.ExitCode,
	}, nil
}

// execWaitTimeout bounds how long aishwnd waits for aishwin's exec_result
// before giving up on the wire round trip. Background requests reply almost
// immediately (spawning, not completion) so get a short fixed bound;
// foreground requests are bounded by the command's own timeout (which aishwin
// enforces itself and replies promptly after) plus a buffer for wire
// latency — this should essentially never fire in practice.
func execWaitTimeout(args execArgs) time.Duration {
	if args.Background {
		return 15 * time.Second
	}
	ms := args.TimeoutMs
	if ms <= 0 {
		ms = 30000
	}
	return time.Duration(ms)*time.Millisecond + 10*time.Second
}
