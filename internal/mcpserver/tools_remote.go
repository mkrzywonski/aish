package mcpserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ai-ssh/internal/sshmux"
)

// Routed tools: file and exec operations that target "wherever the shared
// session currently is" — the remote host over the ControlMaster channel
// when the user is ssh'd somewhere, the local machine otherwise, and the
// interactive terminal itself (in-band) as a last resort on remotes without
// an out-of-band channel.

func registerRemoteTools(s *mcp.Server, c *Core) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "file_read",
		Description: "Read a file from the host the shared session is currently on (remote when ssh'd, local otherwise). " +
			"Out-of-band: does not disturb the interactive terminal when a multiplexed ssh channel is available.",
	}, c.fileRead)

	mcp.AddTool(s, &mcp.Tool{
		Name: "file_write",
		Description: "Write (or append to) a file on the host the shared session is currently on. " +
			"Content is UTF-8, or base64 with encoding=base64.",
	}, c.fileWrite)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "file_upload",
		Description: "Copy a file from the local machine to the remote host of the current ssh session (multiplexed, no re-auth).",
	}, c.fileUpload)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "file_download",
		Description: "Copy a file from the remote host of the current ssh session to the local machine (multiplexed, no re-auth).",
	}, c.fileDownload)

	mcp.AddTool(s, &mcp.Tool{
		Name: "exec",
		Description: "Run a command out-of-band on the host the shared session is currently on — it does NOT appear in the " +
			"shared terminal. Use background=true for long-running commands, then poll exec_status. " +
			"For commands the user should see, use run_command instead.",
	}, c.execTool)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "exec_status",
		Description: "Poll a background task started by exec: incremental output (pass next_cursor back), running state, exit code.",
	}, c.execStatus)
}

// route decides where file/exec operations go.
type route struct {
	via  string // "local" | "controlmaster" | "in_band"
	ci   *sshmux.ConnInfo
	host string
}

func (c *Core) route() route {
	if ci := c.Mux.Current(); ci != nil {
		if c.Mux.SocketLive(ci) {
			return route{via: "controlmaster", ci: ci, host: ci.Host}
		}
		// ssh is up but no usable master (user override, server refusal):
		// fall back to typing through the shared terminal.
		return route{via: "in_band", host: ci.Host}
	}
	// No shim-tracked ssh. If the foreground process is an ssh the shim
	// didn't see (or OSC 7 reports a foreign host), stay honest: in-band.
	if fg := c.Tracker.Foreground(); fg != nil && fg.Comm == "ssh" {
		return route{via: "in_band", host: "unknown-remote"}
	}
	if h, _ := c.Tracker.Cwd(); h != "" && h != c.Tracker.LocalHost() {
		return route{via: "in_band", host: h}
	}
	return route{via: "local", host: "local"}
}

const (
	maxFileRead   = 256 << 10 // default cap for file_read content
	maxInBand     = 48 << 10  // in-band transfers are size-limited
	execOutputCap = 64 << 10
)

// ---- file_read ----

type fileReadArgs struct {
	Path     string `json:"path" jsonschema:"absolute or ~-relative path on the current host"`
	MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"cap returned content (default 262144)"`
	Offset   int64  `json:"offset,omitempty" jsonschema:"byte offset to start reading from"`
}

type fileReadResult struct {
	Content  string `json:"content"`
	Encoding string `json:"encoding"` // utf8 | base64
	Eof      bool   `json:"eof"`
	Via      string `json:"via"`
	Host     string `json:"host"`
}

func (c *Core) fileRead(ctx context.Context, req *mcp.CallToolRequest, args fileReadArgs) (*mcp.CallToolResult, fileReadResult, error) {
	max := args.MaxBytes
	if max <= 0 {
		max = maxFileRead
	}
	rt := c.route()
	var data []byte
	var eof bool

	switch rt.via {
	case "local":
		f, err := os.Open(expandLocal(args.Path))
		if err != nil {
			return nil, fileReadResult{}, err
		}
		defer f.Close()
		if args.Offset > 0 {
			if _, err := f.Seek(args.Offset, 0); err != nil {
				return nil, fileReadResult{}, err
			}
		}
		buf := make([]byte, max+1)
		n, _ := readFull(f, buf)
		eof = n <= max
		if n > max {
			n = max
		}
		data = buf[:n]

	case "controlmaster":
		// tail -c +N | head -c M is portable across GNU/busybox/BSD.
		cmd := fmt.Sprintf("tail -c +%d %s | head -c %d", args.Offset+1, sshmux.Quote(args.Path), max+1)
		out, err := c.muxOutput(ctx, rt.ci, cmd, nil)
		if err != nil {
			return nil, fileReadResult{}, err
		}
		eof = len(out) <= max
		if len(out) > max {
			out = out[:max]
		}
		data = out

	case "in_band":
		if max > maxInBand {
			max = maxInBand
		}
		cmd := fmt.Sprintf("tail -c +%d %s | head -c %d | base64", args.Offset+1, sshmux.Quote(args.Path), max+1)
		res, err := c.Engine.RunSentinel(cmd, 30*time.Second)
		if err != nil {
			return nil, fileReadResult{}, err
		}
		out, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(res.Output), ""))
		if err != nil {
			return nil, fileReadResult{}, fmt.Errorf("in-band read failed (output: %.200s)", res.Output)
		}
		eof = len(out) <= max
		if len(out) > max {
			out = out[:max]
		}
		data = out
	}

	res := fileReadResult{Eof: eof, Via: rt.via, Host: rt.host}
	if utf8.Valid(data) {
		res.Content, res.Encoding = string(data), "utf8"
	} else {
		res.Content, res.Encoding = base64.StdEncoding.EncodeToString(data), "base64"
	}
	return nil, res, nil
}

// ---- file_write ----

type fileWriteArgs struct {
	Path     string `json:"path"`
	Content  string `json:"content"`
	Encoding string `json:"encoding,omitempty" jsonschema:"utf8 (default) or base64"`
	Append   bool   `json:"append,omitempty"`
	Mode     string `json:"mode,omitempty" jsonschema:"octal file mode to set, e.g. 0644"`
}

type fileWriteResult struct {
	BytesWritten int    `json:"bytes_written"`
	Via          string `json:"via"`
	Host         string `json:"host"`
}

func (c *Core) fileWrite(ctx context.Context, req *mcp.CallToolRequest, args fileWriteArgs) (*mcp.CallToolResult, fileWriteResult, error) {
	data := []byte(args.Content)
	if args.Encoding == "base64" {
		var err error
		if data, err = base64.StdEncoding.DecodeString(args.Content); err != nil {
			return nil, fileWriteResult{}, err
		}
	}
	rt := c.route()

	switch rt.via {
	case "local":
		path := expandLocal(args.Path)
		flags := os.O_WRONLY | os.O_CREATE
		if args.Append {
			flags |= os.O_APPEND
		} else {
			flags |= os.O_TRUNC
		}
		mode := os.FileMode(0o644)
		f, err := os.OpenFile(path, flags, mode)
		if err != nil {
			return nil, fileWriteResult{}, err
		}
		if _, err := f.Write(data); err != nil {
			f.Close()
			return nil, fileWriteResult{}, err
		}
		f.Close()
		if args.Mode != "" {
			if m, err := parseMode(args.Mode); err == nil {
				os.Chmod(path, m)
			}
		}

	case "controlmaster":
		redir := ">"
		if args.Append {
			redir = ">>"
		}
		cmd := fmt.Sprintf("cat %s %s", redir, sshmux.Quote(args.Path))
		if args.Mode != "" {
			cmd += fmt.Sprintf(" && chmod %s %s", args.Mode, sshmux.Quote(args.Path))
		}
		if _, err := c.muxOutput(ctx, rt.ci, cmd, data); err != nil {
			return nil, fileWriteResult{}, err
		}

	case "in_band":
		if len(data) > maxInBand {
			return nil, fileWriteResult{}, fmt.Errorf("in-band write limited to %d bytes (no multiplexed channel to %s); write a smaller file or reconnect ssh through aish", maxInBand, rt.host)
		}
		redir := ">"
		if args.Append {
			redir = ">>"
		}
		b64 := base64.StdEncoding.EncodeToString(data)
		cmd := fmt.Sprintf("printf '%%s' %s | base64 -d %s %s", sshmux.Quote(b64), redir, sshmux.Quote(args.Path))
		if args.Mode != "" {
			cmd += fmt.Sprintf(" && chmod %s %s", args.Mode, sshmux.Quote(args.Path))
		}
		res, err := c.Engine.RunSentinel(cmd, 30*time.Second)
		if err != nil {
			return nil, fileWriteResult{}, err
		}
		if res.ExitCode == nil || *res.ExitCode != 0 {
			return nil, fileWriteResult{}, fmt.Errorf("in-band write failed: %.200s", res.Output)
		}
	}
	return nil, fileWriteResult{BytesWritten: len(data), Via: rt.via, Host: rt.host}, nil
}

// ---- file_upload / file_download ----

type transferArgs struct {
	LocalPath  string `json:"local_path"`
	RemotePath string `json:"remote_path"`
}

type transferResult struct {
	Bytes int64  `json:"bytes"`
	Host  string `json:"host"`
}

func (c *Core) fileUpload(ctx context.Context, req *mcp.CallToolRequest, args transferArgs) (*mcp.CallToolResult, transferResult, error) {
	rt := c.route()
	if rt.via != "controlmaster" {
		return nil, transferResult{}, errors.New("no multiplexed ssh channel (session is local or channel unavailable); use file_write instead")
	}
	data, err := os.ReadFile(expandLocal(args.LocalPath))
	if err != nil {
		return nil, transferResult{}, err
	}
	cmd := fmt.Sprintf("cat > %s", sshmux.Quote(args.RemotePath))
	if _, err := c.muxOutput(ctx, rt.ci, cmd, data); err != nil {
		return nil, transferResult{}, err
	}
	return nil, transferResult{Bytes: int64(len(data)), Host: rt.host}, nil
}

func (c *Core) fileDownload(ctx context.Context, req *mcp.CallToolRequest, args transferArgs) (*mcp.CallToolResult, transferResult, error) {
	rt := c.route()
	if rt.via != "controlmaster" {
		return nil, transferResult{}, errors.New("no multiplexed ssh channel (session is local or channel unavailable); use file_read instead")
	}
	out, err := c.muxOutput(ctx, rt.ci, "cat "+sshmux.Quote(args.RemotePath), nil)
	if err != nil {
		return nil, transferResult{}, err
	}
	if err := os.WriteFile(expandLocal(args.LocalPath), out, 0o644); err != nil {
		return nil, transferResult{}, err
	}
	return nil, transferResult{Bytes: int64(len(out)), Host: rt.host}, nil
}

// ---- exec / exec_status ----

type execArgs struct {
	Command    string `json:"command"`
	Background bool   `json:"background,omitempty"`
	TimeoutMs  int    `json:"timeout_ms,omitempty" jsonschema:"foreground only; default 30000"`
}

type execResult struct {
	Output   string `json:"output,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
	TaskID   string `json:"task_id,omitempty"`
	TimedOut bool   `json:"timed_out,omitempty"`
	Via      string `json:"via"`
	Host     string `json:"host"`
}

func (c *Core) execTool(ctx context.Context, req *mcp.CallToolRequest, args execArgs) (*mcp.CallToolResult, execResult, error) {
	rt := c.route()

	if rt.via == "in_band" {
		if args.Background {
			return nil, execResult{}, errors.New("no out-of-band channel to the remote; run it in the shared terminal with run_command (e.g. with & for background)")
		}
		res, err := c.Engine.RunSentinel(args.Command, time.Duration(args.TimeoutMs)*time.Millisecond)
		if err != nil {
			return nil, execResult{}, err
		}
		return nil, execResult{Output: res.Output, ExitCode: res.ExitCode, TimedOut: res.TimedOut, Via: "in_band", Host: rt.host}, nil
	}

	if args.Background {
		cmd := c.buildExec(context.Background(), rt, args.Command)
		task, err := c.Tasks.Start(cmd)
		if err != nil {
			return nil, execResult{}, err
		}
		return nil, execResult{TaskID: task.ID, Via: rt.via, Host: rt.host}, nil
	}

	timeout := time.Duration(args.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := c.buildExec(cctx, rt, args.Command)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	res := execResult{Via: rt.via, Host: rt.host, Output: capString(buf.Bytes(), execOutputCap)}
	if cctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		return nil, res, nil
	}
	code := 0
	if err != nil {
		code = 1
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
	}
	res.ExitCode = &code
	return nil, res, nil
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

func (c *Core) execStatus(ctx context.Context, req *mcp.CallToolRequest, args execStatusArgs) (*mcp.CallToolResult, execStatusResult, error) {
	task := c.Tasks.Get(args.TaskID)
	if task == nil {
		return nil, execStatusResult{}, fmt.Errorf("no such task %q", args.TaskID)
	}
	cursor := int64(0)
	if args.Cursor != nil {
		cursor = *args.Cursor
	}
	data, next, _ := task.Out.ReadFrom(cursor, execOutputCap)
	running, exit := task.Status()
	return nil, execStatusResult{Running: running, Output: string(data), NextCursor: next, ExitCode: exit}, nil
}

// ---- helpers ----

// buildExec creates the command for out-of-band execution on the routed target.
func (c *Core) buildExec(ctx context.Context, rt route, command string) *exec.Cmd {
	if rt.via == "controlmaster" {
		return c.Mux.Command(ctx, rt.ci, command)
	}
	return exec.CommandContext(ctx, "/bin/sh", "-c", command)
}

// muxOutput runs a remote command over the mux, feeding stdin, returning stdout.
func (c *Core) muxOutput(ctx context.Context, ci *sshmux.ConnInfo, remoteCmd string, stdin []byte) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := c.Mux.Command(cctx, ci, remoteCmd)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("remote command failed: %v: %.300s", err, errb.String())
	}
	return out.Bytes(), nil
}

func expandLocal(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return home + strings.TrimPrefix(p, "~")
		}
	}
	return p
}

func parseMode(s string) (os.FileMode, error) {
	var m uint32
	if _, err := fmt.Sscanf(s, "%o", &m); err != nil {
		return 0, err
	}
	return os.FileMode(m), nil
}

func capString(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	half := max / 2
	return string(b[:half]) + fmt.Sprintf("\n... [%d bytes truncated] ...\n", len(b)-max) + string(b[len(b)-half:])
}

func readFull(f *os.File, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := f.Read(buf[total:])
		total += n
		if err != nil {
			return total, nil
		}
	}
	return total, nil
}
