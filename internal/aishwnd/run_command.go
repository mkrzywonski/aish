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

// runCommandArgs/runCommandResult and taskStatusArgs/taskStatusResult mirror aish's own
// run_command/task_status schemas (internal/mcpserver/tools_remote.go's runCommandArgs
// etc.) minus the SessionArg routing field — aishwnd doesn't implement
// cross-session forwarding, so declaring a field for it would advertise a
// capability that doesn't exist.
type runCommandArgs struct {
	Command    string `json:"command"`
	Cwd        string `json:"cwd,omitempty" jsonschema:"absolute working directory on the Windows host"`
	Background bool   `json:"background,omitempty"`
	TimeoutMs  int    `json:"timeout_ms,omitempty" jsonschema:"foreground only; default 30000"`
	Shell      string `json:"shell,omitempty" jsonschema:"which persistent shell runs this command -- see the tool description for what's available on this host and the default when omitted"`
}

type runCommandResult struct {
	Output   string `json:"output,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
	Framing     string `json:"framing"`
	CursorStart int64  `json:"cursor_start"`
	CursorEnd   int64  `json:"cursor_end"`
	TaskID   string `json:"task_id,omitempty"`
	TimedOut bool   `json:"timed_out,omitempty"`
	// Truncated says the output was shortened from the middle; both ends are
	// present, so the command's conclusion is never the part that is cut.
	Truncated bool `json:"truncated,omitempty"`
	// OutputPath names a file on the Windows host holding the FULL output, and
	// OutputBytes its size. Deleted when the next command starts.
	OutputPath  string `json:"output_path,omitempty"`
	OutputBytes int64  `json:"output_bytes,omitempty"`
	Shell       string `json:"shell"`
	Via         string `json:"via"`
	Host        string `json:"host"`
}

type taskStatusArgs struct {
	TaskID string `json:"task_id"`
	Cursor *int64 `json:"cursor,omitempty" jsonschema:"pass next_cursor from the previous poll"`
}

type taskStatusResult struct {
	Running    bool   `json:"running"`
	Output     string `json:"output"`
	Truncated  bool   `json:"truncated,omitempty"`
	NextCursor int64  `json:"next_cursor"`
	ExitCode   *int   `json:"exit_code,omitempty"`
}

func registerCommandTools(s *mcp.Server, sess *aishwndSession, availableShells []string, defaultShell string) {
	if defaultShell == "" {
		defaultShell = "powershell"
	}
	if len(availableShells) == 0 {
		availableShells = []string{defaultShell}
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "run_command",
		Annotations: commandTool("Run command on Windows host"),
		Description: "Run a command on the Windows host, visibly — it and its output are mirrored to the human's " +
			"console in real time. This tool is named run_command, not exec, because it is the VISIBLE one: " +
			"across aish, exec means the invisible out-of-band route, and this backend has no such route " +
			"for exec to name — there is no shared terminal here to be invisible relative to, so everything you " +
			"run is seen. Runs against a persistent shell that keeps its working " +
			"directory and environment between calls; set cwd to change directory just for this one command. " +
			"Available shells on this host: " + strings.Join(availableShells, ", ") + " (default " + defaultShell +
			" when shell is omitted; powershell is the more capable option for most tasks, cmd for simpler " +
			"cases). Each shell kind keeps its OWN persistent process with its own working directory and " +
			"environment, independent of the others — " +
			"switching which one you use for one call never loses another kind's state. " +
			"Use background=true for long-running commands, then poll task_status. A foreground command that " +
			"times out kills and replaces that shell's persistent process (state is lost) rather than risk its " +
			"late output corrupting the next command's result. Very large output is shortened from the MIDDLE, " +
			"keeping both ends and setting truncated: a command's conclusion is usually its last line, so the " +
			"end is never the part that gets cut. The FULL output is written to the file named by output_path " +
			"on the Windows host, so nothing is lost -- read it with file_read, or search it with file_grep " +
			"without reading it, which is usually what you want when a few useful lines are buried in noise. " +
			"That file is deleted when the next command starts, so retrieve it BEFORE running anything else.",
	}, sess.runCommandTool)

	mcp.AddTool(s, &mcp.Tool{
		Name: "task_status",
		Description: "Poll a background task by the task_id returned when a command was started with " +
			"background=true (run_command on this Windows backend; exec on the shared-terminal backend): " +
			"incremental output since next_cursor, whether it is still running, and the exit code once it " +
			"finishes. Errors if the task_id is unrecognized.",
		Annotations: readOnlyTool("Poll background command"),
	}, sess.taskStatus)
}

func (s *aishwndSession) runCommandTool(ctx context.Context, req *mcp.CallToolRequest, args runCommandArgs) (*mcp.CallToolResult, runCommandResult, error) {
	if args.Command == "" {
		return nil, runCommandResult{}, errors.New("command must not be empty")
	}

	data, err := json.Marshal(aishwinwire.RunCommandData{
		Command:    args.Command,
		Cwd:        args.Cwd,
		Background: args.Background,
		TimeoutMs:  args.TimeoutMs,
		Shell:      args.Shell,
	})
	if err != nil {
		return nil, runCommandResult{}, err
	}

	raw, err := s.roundTrip("run_command", data, runCommandWaitTimeout(args))
	if err != nil {
		return nil, runCommandResult{}, err
	}
	var res aishwinwire.RunCommandResultData
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, runCommandResult{}, fmt.Errorf("malformed run_command result from the Windows peer: %w", err)
	}
	if res.Error != "" {
		return nil, runCommandResult{}, errors.New(res.Error)
	}
	output, truncated := capOutput(res.Output)
	return nil, runCommandResult{
		Output:      output,
		Truncated:   truncated,
		Framing:     "direct",
		CursorStart: 0,
		CursorEnd:   0,
		OutputPath:  res.OutputPath,
		OutputBytes: res.OutputBytes,
		ExitCode:    res.ExitCode,
		TaskID:      res.TaskID,
		TimedOut:    res.TimedOut,
		Shell:       res.Shell,
		Via:         "aishwin",
		Host:        s.displayHost(),
	}, nil
}

func (s *aishwndSession) taskStatus(ctx context.Context, req *mcp.CallToolRequest, args taskStatusArgs) (*mcp.CallToolResult, taskStatusResult, error) {
	if args.TaskID == "" {
		return nil, taskStatusResult{}, errors.New("task_id must not be empty")
	}
	cursor := int64(0)
	if args.Cursor != nil {
		cursor = *args.Cursor
	}
	data, err := json.Marshal(aishwinwire.TaskPollData{TaskID: args.TaskID, Cursor: cursor})
	if err != nil {
		return nil, taskStatusResult{}, err
	}

	raw, err := s.roundTrip("task_poll", data, 10*time.Second)
	if err != nil {
		return nil, taskStatusResult{}, err
	}
	var res aishwinwire.TaskPollResultData
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, taskStatusResult{}, fmt.Errorf("malformed task_poll result from the Windows peer: %w", err)
	}
	if res.Error != "" {
		return nil, taskStatusResult{}, errors.New(res.Error)
	}
	// A paged stream is trimmed from the front, and the cursor reports only
	// what was actually handed over, so the remainder arrives on the next poll
	// instead of being skipped past.
	output, consumed, truncated := capOutputPrefix(res.Output)
	next := res.NextCursor
	if truncated {
		var from int64
		if args.Cursor != nil && *args.Cursor > 0 {
			from = *args.Cursor
		}
		next = from + int64(consumed)
	}
	return nil, taskStatusResult{
		Running:    res.Running,
		Output:     output,
		Truncated:  truncated,
		NextCursor: next,
		ExitCode:   res.ExitCode,
	}, nil
}

// runCommandWaitTimeout bounds how long aishwnd waits for aishwin's exec_result
// before giving up on the wire round trip. Background requests reply almost
// immediately (spawning, not completion) so get a short fixed bound;
// foreground requests are bounded by the command's own timeout (which aishwin
// enforces itself and replies promptly after) plus a buffer for wire
// latency — this should essentially never fire in practice.
func runCommandWaitTimeout(args runCommandArgs) time.Duration {
	if args.Background {
		return 15 * time.Second
	}
	ms := args.TimeoutMs
	if ms <= 0 {
		ms = 30000
	}
	return time.Duration(ms)*time.Millisecond + 10*time.Second
}
