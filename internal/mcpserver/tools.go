package mcpserver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ai-ssh/internal/framing"
	"ai-ssh/internal/paths"
	"ai-ssh/internal/state"
	"ai-ssh/internal/term"
)

func registerTools(s *mcp.Server, c *Core) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "run_command",
		Description: "Run a shell command in the shared terminal (visible to the user) and return its output and exit code. " +
			"Works transparently whether the session is local or inside ssh. Errors if a full-screen app is active or the " +
			"terminal is waiting for secret input.",
	}, c.runCommand)

	mcp.AddTool(s, &mcp.Tool{
		Name: "send_input",
		Description: "Type raw text into the shared terminal exactly as if the user typed it. " +
			"No newline is added; include \\r to submit a command line. " +
			"Prefer run_command for running commands and capturing their output.",
	}, c.sendInput)

	mcp.AddTool(s, &mcp.Tool{
		Name: "send_keys",
		Description: "Press named keys in the shared terminal. Supported: enter, tab, esc, space, backspace, delete, insert, " +
			"up, down, left, right, home, end, page_up, page_down, f1-f12, ctrl_a..ctrl_z (e.g. ctrl_c).",
	}, c.sendKeys)

	mcp.AddTool(s, &mcp.Tool{
		Name: "read_screen",
		Description: "Read the currently visible terminal screen as rendered plain text, " +
			"with cursor position and whether a full-screen app (alternate screen) is active.",
	}, c.readScreen)

	mcp.AddTool(s, &mcp.Tool{
		Name: "read_output",
		Description: "Read the raw session output stream (scrollback) incrementally. Pass the next_cursor from the previous " +
			"call to get only new output; omit cursor to get the most recent output. Escape sequences are stripped unless raw.",
	}, c.readOutput)

	mcp.AddTool(s, &mcp.Tool{
		Name: "wait_idle",
		Description: "Wait until the terminal has produced no output for idle_ms milliseconds (default 1500), " +
			"or until timeout_ms (default 30000) elapses. Useful after sending input that triggers slow output.",
	}, c.waitIdle)

	mcp.AddTool(s, &mcp.Tool{
		Name: "session_status",
		Description: "Get the status of the shared terminal session: session id, screen size, alternate-screen flag, " +
			"time since last output, and other live aish sessions on this machine.",
	}, c.sessionStatus)
}

// ---- run_command ----

type runCommandArgs struct {
	Command   string `json:"command" jsonschema:"the shell command line to run"`
	TimeoutMs int    `json:"timeout_ms,omitempty" jsonschema:"give up waiting after this long (default 30000); the command keeps running"`
}

func (c *Core) runCommand(ctx context.Context, req *mcp.CallToolRequest, args runCommandArgs) (*mcp.CallToolResult, *framing.Result, error) {
	res, err := c.Engine.Run(args.Command, time.Duration(args.TimeoutMs)*time.Millisecond)
	if err != nil {
		return nil, nil, err
	}
	return nil, res, nil
}

// ---- send_input ----

type sendInputArgs struct {
	Text string `json:"text" jsonschema:"the exact bytes to type into the terminal"`
}

type sendInputResult struct {
	BytesWritten int `json:"bytes_written"`
}

func (c *Core) sendInput(ctx context.Context, req *mcp.CallToolRequest, args sendInputArgs) (*mcp.CallToolResult, sendInputResult, error) {
	n, err := c.Sess.WriteInput([]byte(args.Text))
	if err != nil {
		return nil, sendInputResult{}, err
	}
	return nil, sendInputResult{BytesWritten: n}, nil
}

// ---- send_keys ----

type sendKeysArgs struct {
	Keys []string `json:"keys" jsonschema:"named keys to press, in order"`
}

type sendKeysResult struct {
	Ok bool `json:"ok"`
}

var keyMap = map[string]string{
	"enter": "\r", "return": "\r", "tab": "\t", "esc": "\x1b", "escape": "\x1b",
	"space": " ", "backspace": "\x7f",
	"up": "\x1b[A", "down": "\x1b[B", "right": "\x1b[C", "left": "\x1b[D",
	"home": "\x1b[H", "end": "\x1b[F",
	"insert": "\x1b[2~", "delete": "\x1b[3~",
	"page_up": "\x1b[5~", "page_down": "\x1b[6~",
	"f1": "\x1bOP", "f2": "\x1bOQ", "f3": "\x1bOR", "f4": "\x1bOS",
	"f5": "\x1b[15~", "f6": "\x1b[17~", "f7": "\x1b[18~", "f8": "\x1b[19~",
	"f9": "\x1b[20~", "f10": "\x1b[21~", "f11": "\x1b[23~", "f12": "\x1b[24~",
}

func keyBytes(name string) (string, error) {
	if s, ok := keyMap[name]; ok {
		return s, nil
	}
	if len(name) == 6 && name[:5] == "ctrl_" && name[5] >= 'a' && name[5] <= 'z' {
		return string(rune(name[5] - 'a' + 1)), nil
	}
	return "", fmt.Errorf("unknown key %q", name)
}

func (c *Core) sendKeys(ctx context.Context, req *mcp.CallToolRequest, args sendKeysArgs) (*mcp.CallToolResult, sendKeysResult, error) {
	var seq []byte
	for _, k := range args.Keys {
		b, err := keyBytes(k)
		if err != nil {
			return nil, sendKeysResult{}, err
		}
		seq = append(seq, b...)
	}
	if _, err := c.Sess.WriteInput(seq); err != nil {
		return nil, sendKeysResult{}, err
	}
	return nil, sendKeysResult{Ok: true}, nil
}

// ---- read_screen ----

type readScreenArgs struct {
	Raw bool `json:"raw,omitempty" jsonschema:"reserved; plain text is always returned currently"`
}

func (c *Core) readScreen(ctx context.Context, req *mcp.CallToolRequest, args readScreenArgs) (*mcp.CallToolResult, term.Snapshot, error) {
	return nil, c.Term.Screen.Snapshot(), nil
}

// ---- read_output ----

type readOutputArgs struct {
	Cursor   *int64 `json:"cursor,omitempty" jsonschema:"absolute stream offset to read from; omit for the most recent output"`
	MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"maximum bytes to return (default 65536)"`
	Raw      bool   `json:"raw,omitempty" jsonschema:"return verbatim bytes including escape sequences"`
}

type readOutputResult struct {
	Data         string `json:"data"`
	NextCursor   int64  `json:"next_cursor"`
	DroppedBytes int64  `json:"dropped_bytes"`
}

func (c *Core) readOutput(ctx context.Context, req *mcp.CallToolRequest, args readOutputArgs) (*mcp.CallToolResult, readOutputResult, error) {
	max := args.MaxBytes
	if max <= 0 {
		max = 64 << 10
	}
	cursor := int64(-1)
	if args.Cursor != nil {
		cursor = *args.Cursor
	}
	data, next, dropped := c.Term.Ring.ReadFrom(cursor, max)
	if !args.Raw {
		data = term.StripEscapes(data)
	}
	return nil, readOutputResult{Data: string(data), NextCursor: next, DroppedBytes: dropped}, nil
}

// ---- wait_idle ----

type waitIdleArgs struct {
	IdleMs    int `json:"idle_ms,omitempty" jsonschema:"quiet period that counts as idle (default 1500)"`
	TimeoutMs int `json:"timeout_ms,omitempty" jsonschema:"give up after this long (default 30000)"`
}

type waitIdleResult struct {
	Idle     bool  `json:"idle"`
	WaitedMs int64 `json:"waited_ms"`
}

func (c *Core) waitIdle(ctx context.Context, req *mcp.CallToolRequest, args waitIdleArgs) (*mcp.CallToolResult, waitIdleResult, error) {
	idle := time.Duration(args.IdleMs) * time.Millisecond
	if idle <= 0 {
		idle = 1500 * time.Millisecond
	}
	timeout := time.Duration(args.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	start := time.Now()
	deadline := start.Add(timeout)
	for {
		last := time.Unix(0, c.Sess.LastOutputNanos())
		quiet := time.Since(last)
		if c.Sess.LastOutputNanos() == 0 {
			quiet = time.Since(start)
		}
		if quiet >= idle {
			return nil, waitIdleResult{Idle: true, WaitedMs: time.Since(start).Milliseconds()}, nil
		}
		if time.Now().After(deadline) {
			return nil, waitIdleResult{Idle: false, WaitedMs: time.Since(start).Milliseconds()}, nil
		}
		wait := idle - quiet
		if wait > 100*time.Millisecond {
			wait = 100 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return nil, waitIdleResult{}, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// ---- session_status ----

type sessionStatusArgs struct{}

type sessionStatusResult struct {
	SessionID     string            `json:"session_id"`
	OtherSessions []string          `json:"other_sessions"`
	Mode          string            `json:"mode"`
	Host          string            `json:"host"`
	OobVia        string            `json:"oob_via"`
	SSHUser       string            `json:"ssh_user,omitempty"`
	Cwd           string            `json:"cwd,omitempty"`
	PromptReady   bool              `json:"prompt_ready"`
	EchoOff       bool              `json:"echo_off"`
	Foreground    *state.Foreground `json:"foreground,omitempty"`
	Rows          int               `json:"rows"`
	Cols          int               `json:"cols"`
	AltScreen     bool              `json:"alt_screen"`
	LastOutputMs  int64             `json:"last_output_ms_ago"`
	Ended         bool              `json:"ended"`
}

func (c *Core) sessionStatus(ctx context.Context, req *mcp.CallToolRequest, args sessionStatusArgs) (*mcp.CallToolResult, sessionStatusResult, error) {
	snap := c.Term.Screen.Snapshot()
	_, cwd := c.Tracker.Cwd()
	rt := c.route()
	var sshUser string
	if rt.ci != nil {
		sshUser = rt.ci.User
	}
	res := sessionStatusResult{
		SessionID:   c.Sess.ID,
		Mode:        string(c.Tracker.Mode(snap.AltScreen)),
		Host:        rt.host,
		OobVia:      rt.via,
		SSHUser:     sshUser,
		Cwd:         cwd,
		PromptReady: c.Tracker.PromptReady(),
		EchoOff:     c.Tracker.EchoOff(),
		Foreground:  c.Tracker.Foreground(),
		Rows:        snap.Rows,
		Cols:        snap.Cols,
		AltScreen:   snap.AltScreen,
		Ended:       c.Sess.Closed(),
	}
	if n := c.Sess.LastOutputNanos(); n > 0 {
		res.LastOutputMs = time.Since(time.Unix(0, n)).Milliseconds()
	}
	entries, err := os.ReadDir(paths.Base())
	if err == nil {
		for _, e := range entries {
			if e.IsDir() && e.Name() != c.Sess.ID {
				if _, err := os.Stat(filepath.Join(paths.Base(), e.Name(), "mcp.sock")); err == nil {
					res.OtherSessions = append(res.OtherSessions, e.Name())
				}
			}
		}
	}
	return nil, res, nil
}
