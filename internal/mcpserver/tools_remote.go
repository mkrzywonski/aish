package mcpserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
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
		Name:        "file_read",
		Annotations: readOnlyTool("Read file on session host"),
		Description: "Read a file from the host the shared session is currently on (remote when ssh'd, local otherwise). " +
			"Out-of-band (invisible) when authorized and a route is available; " +
			"otherwise it works by typing through the shared terminal (visible to the user, size-limited). " +
			"Non-UTF-8 content is returned base64 (see encoding).",
	}, c.fileRead)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "file_write",
		Annotations: mutatingTool("Write file on session host", true, false),
		Description: "Write (or append to) a file on the host the shared session is currently on. " +
			"Content is UTF-8, or base64 with encoding=base64. Out-of-band (invisible) when authorized; " +
			"otherwise the write types base64 through the shared terminal (visible to the user) " +
			"and is limited to 48KB.",
	}, c.fileWrite)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "file_edit",
		Annotations: mutatingTool("Edit file on session host", true, false),
		Description: "Edit a UTF-8 text file on the session's current host by replacing exact text. Fails when old_text " +
			"is absent or occurs more than once unless replace_all=true. Requires an authorized local or remote OOB route " +
			"and never types an editing wrapper into the shared terminal.",
	}, c.fileEdit)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "file_stat",
		Annotations: readOnlyTool("Inspect path on session host"),
		Description: "Inspect an absolute path on the session's current host. Returns its type, size, permissions, and " +
			"modification time without following a symbolic link. Requires an authorized local or remote OOB route.",
	}, c.fileStat)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "directory_list",
		Annotations: readOnlyTool("List directory on session host"),
		Description: "List direct children of an absolute directory on the session's current host, sorted by name, with " +
			"type, size, and modification time. Requires an authorized local or remote OOB route.",
	}, c.directoryList)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "file_upload",
		Annotations: mutatingTool("Upload file to session host", true, false),
		Description: "Copy a local file to the remote host of the current SSH session over its authorized OOB channel. " +
			"The persistent channel may require approval when first opened on MFA-protected hosts. Errors when the session " +
			"is local or no multiplexed channel is available — " +
			"use file_write then.",
	}, c.fileUpload)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "file_download",
		Annotations: mutatingTool("Download file from session host", true, false),
		Description: "Copy a file from the remote host of the current SSH session to the local machine over its authorized " +
			"OOB channel. The persistent channel may require approval when first opened on MFA-protected hosts. Errors when " +
			"the session is local or no multiplexed channel is available — " +
			"use file_read then.",
	}, c.fileDownload)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "exec",
		Annotations: commandTool("Execute on session host"),
		Description: "Run a command on the host the shared session is currently on. Invisible out-of-band execution " +
			"requires authorization and an OOB route; " +
			"otherwise the command runs in-band, visibly, through the shared terminal. Use background=true for " +
			"long-running commands, then poll exec_status (background requires an OOB route). Set cwd to an absolute " +
			"directory when the command must run somewhere other than the OOB shell's default directory. " +
			"For commands the user should see, prefer run_command.",
	}, c.execTool)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "exec_status",
		Description: "Poll a background task started by exec: incremental output (pass next_cursor back), running state, exit code.",
		Annotations: readOnlyTool("Poll background command"),
	}, c.execStatus)
}

// route decides where file/exec operations go.
type route struct {
	via  string // "local" | "controlmaster" | "in_band"
	ci   *sshmux.ConnInfo
	host string
}

// capability reports where an out-of-band operation COULD go, ignoring
// authorization policy: the live ControlMaster channel when the user is
// ssh'd somewhere with multiplexing up, the local machine when the session
// is local, and the interactive terminal (in_band) for remotes with no
// usable master. It never prompts — session_status and route() both use it.
func (c *Core) capability() route {
	if ci := c.Mux.Current(); ci != nil {
		if c.Mux.SocketLive(ci) {
			return route{via: "controlmaster", ci: ci, host: ci.Host}
		}
		// ssh is up but no usable master (not authorized at connect, user
		// override, server refusal): typing through the shared terminal.
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

// route resolves where an actual file/exec operation goes, applying the
// out-of-band authorization policy. in_band (visible) is always allowed. An
// OOB-capable path (controlmaster/local) is used directly when OOB is
// granted; otherwise the user is asked y/n/a in the terminal — yes allows
// this op, always grants the session, no (or timeout) downgrades to the
// visible in-band path. This is the only routing entry point that may block
// on a prompt, so only real operations call it (never session_status).
func (c *Core) route() route {
	cap := c.capability()
	if cap.via == "in_band" || c.oobGranted() {
		return cap
	}
	ans, ok := c.Sess.Prompt(
		fmt.Sprintf("the AI wants out-of-band (invisible) access on %s — allow?", cap.host),
		"yna", 120*time.Second)
	switch {
	case ok && ans == 'a':
		c.grantOOBAlways()
		c.Sess.Notify("out-of-band access granted for this session.")
		return cap
	case ok && ans == 'y':
		return cap
	default:
		// no / timeout: do it visibly through the shared terminal instead.
		return c.downgrade(cap)
	}
}

// hostConfidence compares where the interactive tty appears to be (the OSC7
// host of the shell) against where an out-of-band op on rt would land. It is
// the targeting-confidence signal reported by session_status and (in a later
// phase) enforced by the write path:
//
//	same     — a local session, or the OSC7 host matches the probed OOB host
//	mismatch — OSC7 host is known and differs from the probed OOB host
//	unknown  — OSC7 host unreported, or the OOB host not yet probed (can't verify)
//
// Remote comparison is against the *probed* hostname, never the ssh target in
// ci.Host: that target may be an alias ("ssh web") whose real hostname differs,
// which would read as a false mismatch. When caps aren't probed yet we report
// unknown rather than guess.
func (c *Core) hostConfidence(rt route) (interactiveHost, oobHost, confidence string) {
	ttyHost, _ := c.Tracker.Cwd()
	interactiveHost = ttyHost

	if rt.via == "local" {
		lh := c.Tracker.LocalHost()
		if interactiveHost == "" {
			interactiveHost = lh
		}
		return interactiveHost, lh, "same"
	}

	oobHost = rt.host
	caps, ok := c.Mux.CachedCapabilities(rt.ci)
	if !ok || caps.Hostname == "" {
		return interactiveHost, oobHost, "unknown"
	}
	oobHost = caps.Hostname
	switch {
	case ttyHost == "":
		return interactiveHost, oobHost, "unknown"
	case ttyHost == caps.Hostname:
		return interactiveHost, oobHost, "same"
	default:
		return interactiveHost, oobHost, "mismatch"
	}
}

// opKind distinguishes read-only from mutating operations for the host
// divergence policy: mutations fail closed on a detected mismatch, reads only
// warn.
type opKind int

const (
	opRead opKind = iota
	opMutate
)

// divergenceAction is the decision of the (pure, testable) divergence policy.
type divergenceAction int

const (
	divAllow   divergenceAction = iota // proceed, no notice
	divWarn                            // proceed, attach a warning (read-only mismatch)
	divConfirm                         // mutating op, uncertain target: ask the user once
	divFail                            // mutating op, detected mismatch: refuse
)

// divergencePolicy is the three-way host-targeting rule agreed with Codex,
// isolated as a pure function so it can be tested without a live channel:
//
//	same     → allow
//	mismatch → fail closed for a mutation, warn for a read
//	unknown  → a mutation needs a one-time confirm (unless already confirmed);
//	           reads proceed silently
func divergencePolicy(confidence string, kind opKind, alreadyConfirmed bool) divergenceAction {
	switch confidence {
	case "mismatch":
		if kind == opMutate {
			return divFail
		}
		return divWarn
	case "unknown":
		if kind == opMutate && !alreadyConfirmed {
			return divConfirm
		}
		return divAllow
	default: // "same"
		return divAllow
	}
}

// guardTarget applies the host-divergence policy to an OOB route before an
// operation runs. Divergence is only possible over the ControlMaster channel (a
// local session is one host; in_band ops go to the visible tty itself), so
// every other route is allowed unchanged. For a mutation it first ensures the
// channel is probed — the same open the write would trigger — so even the first
// write is checked against the real remote host. It returns a warning to attach
// to a read result, or an error that fails the operation closed.
func (c *Core) guardTarget(rt route, kind opKind) (warning string, err error) {
	if rt.via != "controlmaster" {
		return "", nil
	}
	if kind == opMutate {
		// Probe now (opening the channel if needed) so the confidence check sees
		// the real remote hostname rather than "unknown-because-unprobed".
		if _, perr := c.Mux.EnsureProbed(rt.ci); perr != nil {
			return "", perr
		}
	}
	interactiveHost, oobHost, confidence := c.hostConfidence(rt)
	token := rt.host
	if rt.ci != nil && rt.ci.Host != "" {
		token = rt.ci.Host
	}
	switch divergencePolicy(confidence, kind, c.targetConfirmed(token)) {
	case divFail:
		return "", fmt.Errorf(
			"refusing out-of-band write: the interactive shell appears to be on %q but the OOB channel targets %q; reconnect ssh through aish so they match, or use run_command to act visibly on the shell's host",
			interactiveHost, oobHost)
	case divWarn:
		return fmt.Sprintf(
			"host mismatch: this result came from %q (out-of-band target) but the interactive shell appears to be on %q",
			oobHost, interactiveHost), nil
	case divConfirm:
		ans, ok := c.Sess.Prompt(
			fmt.Sprintf("Cannot verify the interactive shell is still on %s. Proceed with OOB write to %s?", oobHost, oobHost),
			"yn", 120*time.Second)
		if ok && ans == 'y' {
			c.confirmTarget(token)
			return "", nil
		}
		return "", fmt.Errorf("out-of-band write to %s not confirmed (its host could not be verified); reconnect ssh through aish, or use run_command", oobHost)
	default:
		return "", nil
	}
}

// downgrade turns an OOB-capable route into its visible in-band equivalent
// (used when the user declines out-of-band access).
func (c *Core) downgrade(cap route) route {
	if cap.via == "controlmaster" {
		return route{via: "in_band", ci: cap.ci, host: cap.host}
	}
	return route{via: "in_band", host: "local"}
}

const (
	maxFileRead   = 256 << 10 // default cap for file_read content
	maxFileEdit   = 1 << 20   // exact-match edits intentionally stay bounded
	maxInBand     = 48 << 10  // in-band transfers are size-limited
	execOutputCap = 64 << 10
)

// ---- file_read ----

type fileReadArgs struct {
	SessionArg
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
	Warning  string `json:"warning,omitempty"`
}

func (c *Core) fileRead(ctx context.Context, req *mcp.CallToolRequest, args fileReadArgs) (*mcp.CallToolResult, fileReadResult, error) {
	max := args.MaxBytes
	if max <= 0 {
		max = maxFileRead
	}
	rt := c.route()
	warning, _ := c.guardTarget(rt, opRead)
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
		// Over the persistent channel; base64 keeps the line-oriented
		// framing binary-safe. tail/head/base64 are portable.
		cmd := fmt.Sprintf("tail -c +%d %s | head -c %d | base64", args.Offset+1, sshmux.Quote(args.Path), max+1)
		out, err := c.channelOutput(rt.ci, cmd, 60*time.Second)
		if err != nil {
			return nil, fileReadResult{}, err
		}
		dec, derr := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(string(out)), ""))
		if derr != nil {
			return nil, fileReadResult{}, fmt.Errorf("oob channel read failed (output: %.200s)", out)
		}
		eof = len(dec) <= max
		if len(dec) > max {
			dec = dec[:max]
		}
		data = dec

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

	via := rt.via
	if via == "controlmaster" {
		via = "channel" // shared persistent channel, not a fresh one per op
	}
	res := fileReadResult{Eof: eof, Via: via, Host: rt.host, Warning: warning}
	if utf8.Valid(data) {
		res.Content, res.Encoding = string(data), "utf8"
	} else {
		res.Content, res.Encoding = base64.StdEncoding.EncodeToString(data), "base64"
	}
	return nil, res, nil
}

// ---- file_write ----

type fileWriteArgs struct {
	SessionArg
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
	if _, err := c.guardTarget(rt, opMutate); err != nil {
		return nil, fileWriteResult{}, err
	}

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
		res, err := c.Mux.ChannelRun(rt.ci, sshmux.WriteScript(args.Path, data, args.Append, args.Mode), 60*time.Second)
		if err != nil {
			return nil, fileWriteResult{}, err
		}
		if res.TimedOut {
			return nil, fileWriteResult{}, errors.New("oob channel write timed out")
		}
		if res.Exit != 0 {
			return nil, fileWriteResult{}, fmt.Errorf("oob channel write failed: %.300s", res.Output)
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
	via := rt.via
	if via == "controlmaster" {
		via = "channel"
	}
	return nil, fileWriteResult{BytesWritten: len(data), Via: via, Host: rt.host}, nil
}

// ---- file_edit ----

type fileEditArgs struct {
	SessionArg
	Path       string `json:"path" jsonschema:"absolute path on the current host"`
	OldText    string `json:"old_text" jsonschema:"exact text to replace; must be unique unless replace_all"`
	NewText    string `json:"new_text"`
	ReplaceAll bool   `json:"replace_all,omitempty" jsonschema:"replace every occurrence instead of requiring a unique match"`
}

type fileEditResult struct {
	Replacements int    `json:"replacements"`
	BytesWritten int    `json:"bytes_written"`
	Via          string `json:"via"`
	Host         string `json:"host"`
}

func (c *Core) fileEdit(ctx context.Context, req *mcp.CallToolRequest, args fileEditArgs) (*mcp.CallToolResult, fileEditResult, error) {
	if err := validateAbsolutePath(args.Path); err != nil {
		return nil, fileEditResult{}, err
	}
	if args.OldText == "" {
		return nil, fileEditResult{}, errors.New("old_text must not be empty")
	}
	rt := c.route()
	if rt.via == "in_band" {
		return nil, fileEditResult{}, oobPrimitiveError("file_edit", rt.host)
	}
	if _, err := c.guardTarget(rt, opMutate); err != nil {
		return nil, fileEditResult{}, err
	}

	data, err := c.readOOBFile(rt, args.Path, maxFileEdit)
	if err != nil {
		return nil, fileEditResult{}, err
	}
	if !utf8.Valid(data) {
		return nil, fileEditResult{}, errors.New("file_edit requires a UTF-8 text file; use file_read/file_write with base64 for binary data")
	}
	count := strings.Count(string(data), args.OldText)
	switch {
	case count == 0:
		return nil, fileEditResult{}, errors.New("old_text was not found; read the file again and use an exact current match")
	case count > 1 && !args.ReplaceAll:
		return nil, fileEditResult{}, fmt.Errorf("old_text occurs %d times; provide a larger unique match or set replace_all=true", count)
	}
	n := 1
	if args.ReplaceAll {
		n = -1
	}
	updated := []byte(strings.Replace(string(data), args.OldText, args.NewText, n))
	if len(updated) > maxFileEdit {
		return nil, fileEditResult{}, fmt.Errorf("edited file would exceed file_edit limit of %d bytes", maxFileEdit)
	}
	if err := c.writeOOBFile(rt, args.Path, updated); err != nil {
		return nil, fileEditResult{}, err
	}
	if !args.ReplaceAll {
		count = 1
	}
	return nil, fileEditResult{
		Replacements: count,
		BytesWritten: len(updated),
		Via:          resultVia(rt),
		Host:         rt.host,
	}, nil
}

// ---- file_stat ----

type fileStatArgs struct {
	SessionArg
	Path string `json:"path" jsonschema:"absolute path on the current host"`
}

type fileStatResult struct {
	Path         string `json:"path"`
	Type         string `json:"type"`
	Size         int64  `json:"size"`
	Mode         string `json:"mode"`
	ModifiedUnix int64  `json:"modified_unix"`
	Via          string `json:"via"`
	Host         string `json:"host"`
	Warning      string `json:"warning,omitempty"`
}

func (c *Core) fileStat(ctx context.Context, req *mcp.CallToolRequest, args fileStatArgs) (*mcp.CallToolResult, fileStatResult, error) {
	if err := validateAbsolutePath(args.Path); err != nil {
		return nil, fileStatResult{}, err
	}
	rt := c.route()
	if rt.via == "in_band" {
		return nil, fileStatResult{}, oobPrimitiveError("file_stat", rt.host)
	}
	warning, _ := c.guardTarget(rt, opRead)

	res := fileStatResult{Path: args.Path, Via: resultVia(rt), Host: rt.host, Warning: warning}
	if rt.via == "local" {
		info, err := os.Lstat(args.Path)
		if err != nil {
			return nil, fileStatResult{}, err
		}
		res.Type = localFileType(info.Mode())
		res.Size = info.Size()
		res.Mode = fmt.Sprintf("%04o", info.Mode().Perm())
		res.ModifiedUnix = info.ModTime().Unix()
		return nil, res, nil
	}

	cmd := "stat --printf='%a\\t%s\\t%Y\\t%F' -- " + sshmux.Quote(args.Path)
	out, err := c.channelOutput(rt.ci, cmd, 30*time.Second)
	if err != nil {
		return nil, fileStatResult{}, err
	}
	parts := strings.SplitN(strings.TrimSuffix(string(out), "\n"), "\t", 4)
	if len(parts) != 4 {
		return nil, fileStatResult{}, fmt.Errorf("unexpected stat response: %.200s", out)
	}
	res.Mode = normalizePermissions(parts[0])
	res.Size, err = strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return nil, fileStatResult{}, fmt.Errorf("invalid stat size %q", parts[1])
	}
	res.ModifiedUnix, err = strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return nil, fileStatResult{}, fmt.Errorf("invalid stat timestamp %q", parts[2])
	}
	res.Type = remoteFileType(parts[3])
	return nil, res, nil
}

// ---- directory_list ----

type directoryListArgs struct {
	SessionArg
	Path       string `json:"path" jsonschema:"absolute directory path on the current host"`
	MaxEntries int    `json:"max_entries,omitempty" jsonschema:"maximum entries to return (default 1000, maximum 10000)"`
}

type directoryEntry struct {
	Name         string `json:"name"`
	Type         string `json:"type"`
	Size         int64  `json:"size"`
	ModifiedUnix int64  `json:"modified_unix"`
}

type directoryListResult struct {
	Entries   []directoryEntry `json:"entries"`
	Truncated bool             `json:"truncated"`
	Via       string           `json:"via"`
	Host      string           `json:"host"`
	Warning   string           `json:"warning,omitempty"`
}

func (c *Core) directoryList(ctx context.Context, req *mcp.CallToolRequest, args directoryListArgs) (*mcp.CallToolResult, directoryListResult, error) {
	if err := validateAbsolutePath(args.Path); err != nil {
		return nil, directoryListResult{}, err
	}
	max := args.MaxEntries
	if max <= 0 {
		max = 1000
	}
	if max > 10000 {
		return nil, directoryListResult{}, errors.New("max_entries must not exceed 10000")
	}
	rt := c.route()
	if rt.via == "in_band" {
		return nil, directoryListResult{}, oobPrimitiveError("directory_list", rt.host)
	}
	warning, _ := c.guardTarget(rt, opRead)

	res := directoryListResult{Via: resultVia(rt), Host: rt.host, Warning: warning}
	if rt.via == "local" {
		entries, err := os.ReadDir(args.Path)
		if err != nil {
			return nil, directoryListResult{}, err
		}
		res.Truncated = len(entries) > max
		if len(entries) > max {
			entries = entries[:max]
		}
		for _, entry := range entries {
			info, err := entry.Info()
			if err != nil {
				return nil, directoryListResult{}, fmt.Errorf("stat %s: %w", entry.Name(), err)
			}
			res.Entries = append(res.Entries, directoryEntry{
				Name: entry.Name(), Type: localFileType(info.Mode()), Size: info.Size(), ModifiedUnix: info.ModTime().Unix(),
			})
		}
		return nil, res, nil
	}

	// GNU find/head are available on the Linux hosts aish supports. NUL fields
	// keep names containing tabs or newlines unambiguous.
	fieldLimit := (max + 1) * 4
	cmd := fmt.Sprintf("test -d %s && find %s -mindepth 1 -maxdepth 1 -printf '%%f\\0%%y\\0%%s\\0%%T@\\0' | head -z -n %d",
		sshmux.Quote(args.Path), sshmux.Quote(args.Path), fieldLimit)
	out, err := c.channelOutput(rt.ci, cmd, 60*time.Second)
	if err != nil {
		return nil, directoryListResult{}, err
	}
	parts := bytes.Split(out, []byte{0})
	if len(parts) > 0 && len(parts[len(parts)-1]) == 0 {
		parts = parts[:len(parts)-1]
	}
	if len(parts)%4 != 0 {
		return nil, directoryListResult{}, errors.New("unexpected directory listing response")
	}
	for i := 0; i < len(parts); i += 4 {
		size, serr := strconv.ParseInt(string(parts[i+2]), 10, 64)
		mtime, merr := strconv.ParseFloat(string(parts[i+3]), 64)
		if serr != nil || merr != nil {
			return nil, directoryListResult{}, fmt.Errorf("invalid directory entry metadata for %q", parts[i])
		}
		res.Entries = append(res.Entries, directoryEntry{
			Name: string(parts[i]), Type: findFileType(string(parts[i+1])), Size: size, ModifiedUnix: int64(mtime),
		})
	}
	sort.Slice(res.Entries, func(i, j int) bool { return res.Entries[i].Name < res.Entries[j].Name })
	res.Truncated = len(res.Entries) > max
	if len(res.Entries) > max {
		res.Entries = res.Entries[:max]
	}
	return nil, res, nil
}

// ---- file_upload / file_download ----

type transferArgs struct {
	SessionArg
	LocalPath  string `json:"local_path"`
	RemotePath string `json:"remote_path"`
}

type transferResult struct {
	Bytes   int64  `json:"bytes"`
	Host    string `json:"host"`
	Warning string `json:"warning,omitempty"`
}

func (c *Core) fileUpload(ctx context.Context, req *mcp.CallToolRequest, args transferArgs) (*mcp.CallToolResult, transferResult, error) {
	rt := c.route()
	if rt.via != "controlmaster" {
		return nil, transferResult{}, errors.New("no authorized multiplexed SSH channel (session is local, OOB was not enabled before SSH, or the channel is unavailable); use file_write instead")
	}
	if _, err := c.guardTarget(rt, opMutate); err != nil {
		return nil, transferResult{}, err
	}
	data, err := os.ReadFile(expandLocal(args.LocalPath))
	if err != nil {
		return nil, transferResult{}, err
	}
	res, err := c.Mux.ChannelRun(rt.ci, sshmux.WriteScript(args.RemotePath, data, false, ""), 120*time.Second)
	if err != nil {
		return nil, transferResult{}, err
	}
	if res.TimedOut || res.Exit != 0 {
		return nil, transferResult{}, fmt.Errorf("oob channel upload failed: %.300s", res.Output)
	}
	return nil, transferResult{Bytes: int64(len(data)), Host: rt.host}, nil
}

func (c *Core) fileDownload(ctx context.Context, req *mcp.CallToolRequest, args transferArgs) (*mcp.CallToolResult, transferResult, error) {
	rt := c.route()
	if rt.via != "controlmaster" {
		return nil, transferResult{}, errors.New("no authorized multiplexed SSH channel (session is local, OOB was not enabled before SSH, or the channel is unavailable); use file_read instead")
	}
	warning, _ := c.guardTarget(rt, opRead)
	out, err := c.channelOutput(rt.ci, "base64 < "+sshmux.Quote(args.RemotePath), 120*time.Second)
	if err != nil {
		return nil, transferResult{}, err
	}
	dec, derr := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(string(out)), ""))
	if derr != nil {
		return nil, transferResult{}, fmt.Errorf("oob channel download failed (output: %.200s)", out)
	}
	if err := os.WriteFile(expandLocal(args.LocalPath), dec, 0o644); err != nil {
		return nil, transferResult{}, err
	}
	return nil, transferResult{Bytes: int64(len(dec)), Host: rt.host, Warning: warning}, nil
}

// ---- exec / exec_status ----

type execArgs struct {
	SessionArg
	Command    string `json:"command"`
	Cwd        string `json:"cwd,omitempty" jsonschema:"absolute working directory on the current host"`
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
	if args.Cwd != "" {
		if err := validateAbsolutePath(args.Cwd); err != nil {
			return nil, execResult{}, fmt.Errorf("cwd: %w", err)
		}
	}
	rt := c.route()
	if _, err := c.guardTarget(rt, opMutate); err != nil {
		return nil, execResult{}, err
	}

	if rt.via == "in_band" {
		if args.Background {
			return nil, execResult{}, errors.New("no authorized out-of-band route (enable OOB before entering SSH); run it in the shared terminal with run_command, for example with & for background")
		}
		res, err := c.Engine.RunSentinel(commandWithCwd(args.Command, args.Cwd), time.Duration(args.TimeoutMs)*time.Millisecond)
		if err != nil {
			return nil, execResult{}, err
		}
		return nil, execResult{Output: res.Output, ExitCode: res.ExitCode, TimedOut: res.TimedOut, Via: "in_band", Host: rt.host}, nil
	}

	if args.Background {
		// Long-running tasks need a concurrent stream, so they get a
		// dedicated channel (one extra MFA push on strict hosts).
		cmd := c.buildExec(context.Background(), rt, args.Command, args.Cwd)
		task, err := c.Tasks.Start(cmd)
		if err != nil {
			return nil, execResult{}, err
		}
		return nil, execResult{TaskID: task.ID, Via: rt.via, Host: rt.host}, nil
	}

	if rt.via == "controlmaster" {
		timeout := time.Duration(args.TimeoutMs) * time.Millisecond
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		script := "{ " + args.Command + "\n} </dev/null 2>&1"
		if args.Cwd != "" {
			script = "(cd " + sshmux.Quote(args.Cwd) + " && { " + args.Command + "\n}) </dev/null 2>&1"
		}
		cres, err := c.Mux.ChannelRun(rt.ci, script, timeout)
		if err != nil {
			return nil, execResult{}, err
		}
		res := execResult{Via: "channel", Host: rt.host, Output: capString(cres.Output, execOutputCap)}
		if cres.TimedOut {
			res.TimedOut = true
			return nil, res, nil
		}
		exit := cres.Exit
		res.ExitCode = &exit
		return nil, res, nil
	}

	timeout := time.Duration(args.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := c.buildExec(cctx, rt, args.Command, args.Cwd)
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
	SessionArg
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
func (c *Core) buildExec(ctx context.Context, rt route, command, cwd string) *exec.Cmd {
	if rt.via == "controlmaster" {
		return c.Mux.Command(ctx, rt.ci, commandWithCwd(command, cwd))
	}
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	if cwd != "" {
		cmd.Dir = cwd
	}
	return cmd
}

// channelOutput runs a read-type command over the persistent OOB channel
// and returns its stdout (stderr merged; a nonzero exit surfaces it).
func (c *Core) channelOutput(ci *sshmux.ConnInfo, remoteCmd string, timeout time.Duration) ([]byte, error) {
	res, err := c.Mux.ChannelRun(ci, "{ "+remoteCmd+"\n} </dev/null 2>&1", timeout)
	if err != nil {
		return nil, err
	}
	if res.TimedOut {
		return nil, errors.New("oob channel command timed out")
	}
	if res.Exit != 0 {
		return nil, fmt.Errorf("remote command failed (exit %d): %.300s", res.Exit, res.Output)
	}
	return res.Output, nil
}

func (c *Core) readOOBFile(rt route, path string, max int) ([]byte, error) {
	switch rt.via {
	case "local":
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		data := make([]byte, max+1)
		n, _ := readFull(f, data)
		if n > max {
			return nil, fmt.Errorf("file exceeds file_edit limit of %d bytes", max)
		}
		return data[:n], nil
	case "controlmaster":
		cmd := fmt.Sprintf("test -f %s && head -c %d %s | base64", sshmux.Quote(path), max+1, sshmux.Quote(path))
		out, err := c.channelOutput(rt.ci, cmd, 60*time.Second)
		if err != nil {
			return nil, err
		}
		data, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(string(out)), ""))
		if err != nil {
			return nil, fmt.Errorf("decoding remote file: %w", err)
		}
		if len(data) > max {
			return nil, fmt.Errorf("file exceeds file_edit limit of %d bytes", max)
		}
		return data, nil
	default:
		return nil, oobPrimitiveError("file_edit", rt.host)
	}
}

func (c *Core) writeOOBFile(rt route, path string, data []byte) error {
	switch rt.via {
	case "local":
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		return os.WriteFile(path, data, info.Mode().Perm())
	case "controlmaster":
		res, err := c.Mux.ChannelRun(rt.ci, sshmux.WriteScript(path, data, false, ""), 60*time.Second)
		if err != nil {
			return err
		}
		if res.TimedOut {
			return errors.New("oob channel edit timed out")
		}
		if res.Exit != 0 {
			return fmt.Errorf("oob channel edit failed: %.300s", res.Output)
		}
		return nil
	default:
		return oobPrimitiveError("file_edit", rt.host)
	}
}

func validateAbsolutePath(path string) error {
	if path == "" {
		return errors.New("path must not be empty")
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("path %q must be absolute", path)
	}
	return nil
}

func oobPrimitiveError(tool, host string) error {
	return fmt.Errorf("%s requires an authorized out-of-band route to %s; enable OOB before entering SSH, or use run_command for visible terminal work", tool, host)
}

func resultVia(rt route) string {
	if rt.via == "controlmaster" {
		return "channel"
	}
	return rt.via
}

func commandWithCwd(command, cwd string) string {
	if cwd == "" {
		return command
	}
	return "cd " + sshmux.Quote(cwd) + " && " + command
}

func localFileType(mode os.FileMode) string {
	switch {
	case mode.IsRegular():
		return "file"
	case mode.IsDir():
		return "directory"
	case mode&os.ModeSymlink != 0:
		return "symlink"
	case mode&os.ModeNamedPipe != 0:
		return "fifo"
	case mode&os.ModeSocket != 0:
		return "socket"
	case mode&os.ModeDevice != 0 && mode&os.ModeCharDevice != 0:
		return "char_device"
	case mode&os.ModeDevice != 0:
		return "block_device"
	default:
		return "other"
	}
}

func remoteFileType(kind string) string {
	switch strings.TrimSpace(kind) {
	case "regular file", "regular empty file":
		return "file"
	case "directory":
		return "directory"
	case "symbolic link":
		return "symlink"
	case "fifo":
		return "fifo"
	case "socket":
		return "socket"
	case "character special file":
		return "char_device"
	case "block special file":
		return "block_device"
	default:
		return strings.TrimSpace(kind)
	}
}

func findFileType(kind string) string {
	switch kind {
	case "f":
		return "file"
	case "d":
		return "directory"
	case "l":
		return "symlink"
	case "p":
		return "fifo"
	case "s":
		return "socket"
	case "c":
		return "char_device"
	case "b":
		return "block_device"
	default:
		return "other"
	}
}

func normalizePermissions(mode string) string {
	if len(mode) >= 4 {
		return mode
	}
	return strings.Repeat("0", 4-len(mode)) + mode
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
