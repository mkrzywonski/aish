package mcpserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
			"and is limited to 48KB. The file is owned by session_status.oob_user (the SSH login user), not " +
			"whatever user the human's shell is currently on — relevant after su/sudo -i.",
	}, c.fileWrite)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "file_edit",
		Annotations: mutatingTool("Edit file on session host", true, false),
		Description: "Edit a UTF-8 text file on the session's current host by replacing exact text. Fails when old_text " +
			"is absent or occurs more than once unless replace_all=true. Requires an authorized local or remote OOB route " +
			"and never types an editing wrapper into the shared terminal.",
	}, c.fileEdit)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "file_patch",
		Annotations: mutatingTool("Patch file on session host", true, false),
		Description: "Apply a unified diff to a UTF-8 text file on the session's current host, one file per call. Hunks are " +
			"applied inside aish (no remote patch tool needed) and written back atomically. Use file_edit for a single " +
			"exact-text replacement; use file_patch for multi-hunk changes. Optionally pass if_match (a version from " +
			"file_read/file_stat) to apply only if the file is unchanged; otherwise staleness is checked automatically when " +
			"the host has a sha256 tool. Requires an authorized local or remote OOB route.",
	}, c.filePatch)

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
			"Out-of-band, the command runs as session_status.oob_user (the SSH login user) regardless of any su/sudo -i " +
			"in the shared shell; for commands the user should see, or that need the shell's current identity/privileges, " +
			"prefer run_command.",
	}, c.execTool)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "exec_status",
		Description: "Poll a background task started by exec: incremental output (pass next_cursor back), running state, exit code.",
		Annotations: readOnlyTool("Poll background command"),
	}, c.execStatus)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "probe_host",
		Annotations: readOnlyTool("Probe session host tooling"),
		Description: "Initialize the out-of-band toolset on the session's current host: open the OOB channel and run a " +
			"capability probe, then return oob_tools (per-tool availability), remote_capabilities, and target_confidence. " +
			"On a newly-SSH'd host the file/search tools start in state \"unknown\"; call this once to resolve them to " +
			"available/unavailable so you can plan a workflow and, for any unavailable tool, offer to install its package " +
			"(from the install hint) before acting. Unlike session_status this opens the channel, so it may prompt the user " +
			"to authorize out-of-band access, and may cost a one-time MFA prompt on protected hosts. Not needed for local " +
			"sessions. Tools also auto-probe on first use, so this is optional — it just moves the probe earlier for planning.",
	}, c.probeHost)
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
	return interactiveHost, oobHost, classifyConfidence(ttyHost, c.Tracker.LocalHost(), caps.Hostname)
}

// classifyConfidence is the pure host comparison behind hostConfidence,
// isolated for testing. ttyHost is the OSC7-reported interactive host, which
// aish only receives when the shell emits OSC7 — locally always, remotely only
// when the remote shell has integration. remoteHostname is the probed OOB host.
//
// The subtle case is a remote whose shell emits no OSC7 (a lean server, an
// image without vte.sh, a restricted shell): the tracker then still holds the
// stale *local* host from before the ssh. Comparing that against the probed
// remote hostname would read as a false "mismatch" and fail-close every write.
// So an empty or stale-local ttyHost is treated as "unknown" (can't verify —
// the caller confirms once), never "mismatch". A genuine divergence (the remote
// DOES emit OSC7 for a different host) still reads as "mismatch".
func classifyConfidence(ttyHost, localHost, remoteHostname string) string {
	switch {
	case remoteHostname == "":
		return "unknown"
	case ttyHost == remoteHostname:
		return "same"
	case ttyHost == "" || ttyHost == localHost:
		return "unknown"
	default:
		return "mismatch"
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
			fmt.Sprintf("Can't verify the interactive shell is still on %s — [y] write once, [p] set up the aish prompt so I can verify, [n] cancel", oobHost),
			"ypn", 120*time.Second)
		switch {
		case ok && ans == 'p':
			c.ProvisionRemoteTracking()
			c.confirmTarget(token)
			return "", nil
		case ok && ans == 'y':
			c.confirmTarget(token)
			return "", nil
		default:
			return "", fmt.Errorf("out-of-band write to %s not confirmed (its host could not be verified); reconnect ssh through aish, set up the aish prompt from the aish menu, or use run_command", oobHost)
		}
	default:
		return "", nil
	}
}

// ---- probe_host ----

type probeHostArgs struct {
	SessionArg
}

type probeHostResult struct {
	Via              string               `json:"via"`  // local | controlmaster | in_band
	Host             string               `json:"host"` // where OOB ops land
	Probed           bool                 `json:"probed"`
	RemoteHost       *sshmux.Capabilities `json:"remote_capabilities,omitempty"`
	OobTools         map[string]toolAvail `json:"oob_tools"`
	TargetConfidence string               `json:"target_confidence"`
	Note             string               `json:"note,omitempty"`
}

// probeHost is the explicit "reset button": it forces the capability probe on
// the current OOB route so oob_tools resolves from unknown to real states,
// letting the AI plan a workflow (and offer to install any missing package)
// before acting. It routes through route() — probing opens the invisible
// channel, so on an ungranted session it triggers the same OOB consent prompt,
// and on an MFA-protected host it may prompt the human once. Unlike
// session_status (a pure cache reader), this deliberately opens the channel.
func (c *Core) probeHost(ctx context.Context, req *mcp.CallToolRequest, args probeHostArgs) (*mcp.CallToolResult, probeHostResult, error) {
	rt := c.route()
	res := probeHostResult{Via: rt.via, Host: rt.host}
	switch rt.via {
	case "controlmaster":
		caps, err := c.Mux.EnsureProbed(rt.ci)
		if err != nil {
			return nil, probeHostResult{}, err
		}
		res.Probed = true
		res.RemoteHost = &caps
	case "local":
		res.Note = "local session: out-of-band tools run in-process and are always available."
	case "in_band":
		res.Note = "no out-of-band channel to this host; file_read/file_write/exec fall back to the visible terminal and the other tools are unavailable."
	}
	_, _, res.TargetConfidence = c.hostConfidence(rt)
	res.OobTools = c.oobToolAvailability(rt)
	return nil, res, nil
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
	Path        string `json:"path" jsonschema:"absolute or ~-relative path on the current host"`
	MaxBytes    int    `json:"max_bytes,omitempty" jsonschema:"cap returned content (default 262144)"`
	Offset      int64  `json:"offset,omitempty" jsonschema:"byte offset to start reading from"`
	LineNumbers bool   `json:"line_numbers,omitempty" jsonschema:"also return numbered_content (line-numbered, from offset 0 only); content stays raw for file_edit"`
}

type fileReadResult struct {
	Content  string `json:"content"`
	Encoding string `json:"encoding"` // utf8 | base64
	Eof      bool   `json:"eof"`
	// NumberedContent is content with 1-based line numbers, provided only when
	// line_numbers is set and the read started at offset 0. It is for reading
	// and citing lines — never feed it to file_edit/file_patch; use content.
	NumberedContent string `json:"numbered_content,omitempty"`
	// Version is a token for the whole file's current contents (only when the
	// entire file was read); pass it as file_write's if_match to write only if
	// the file hasn't changed since. VersionKind is "sha256".
	Version     string `json:"version,omitempty"`
	VersionKind string `json:"version_kind,omitempty"`
	Via         string `json:"via"`
	Host        string `json:"host"`
	Warning     string `json:"warning,omitempty"`
}

func (c *Core) fileRead(ctx context.Context, req *mcp.CallToolRequest, args fileReadArgs) (*mcp.CallToolResult, fileReadResult, error) {
	max := args.MaxBytes
	if max <= 0 {
		max = maxFileRead
	}
	rt := c.route()
	if err := c.requireTool(rt, "file_read"); err != nil {
		return nil, fileReadResult{}, err
	}
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
	if args.Offset == 0 && eof {
		// The whole file is in hand: a sha256 over these exact bytes is a
		// TOCTOU-correct version token for a later if_match write.
		res.Version, res.VersionKind = sha256Version(data), "sha256"
	}
	if utf8.Valid(data) {
		res.Content, res.Encoding = string(data), "utf8"
		if args.LineNumbers && args.Offset == 0 {
			res.NumberedContent = numberLines(data)
		}
	} else {
		res.Content, res.Encoding = base64.StdEncoding.EncodeToString(data), "base64"
	}
	return nil, res, nil
}

// numberLines renders content with 1-based line numbers (cat -n style), kept
// separate from raw content so line numbers never leak into an edit's old_text.
func numberLines(data []byte) string {
	lines := strings.Split(string(data), "\n")
	// A trailing newline yields a final empty element; don't number it.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	var b strings.Builder
	for i, line := range lines {
		fmt.Fprintf(&b, "%6d\t%s\n", i+1, line)
	}
	return b.String()
}

// ---- file_write ----

type fileWriteArgs struct {
	SessionArg
	Path     string `json:"path"`
	Content  string `json:"content"`
	Encoding string `json:"encoding,omitempty" jsonschema:"utf8 (default) or base64"`
	Append   bool   `json:"append,omitempty"`
	Mode     string `json:"mode,omitempty" jsonschema:"octal file mode to set, e.g. 0644"`
	IfMatch  string `json:"if_match,omitempty" jsonschema:"only write if the file's current version still equals this token (from a prior file_read or file_stat); fails if the file changed. Not valid with append."`
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
	if args.IfMatch != "" && args.Append {
		return nil, fileWriteResult{}, errors.New("if_match cannot be combined with append")
	}
	rt := c.route()
	if _, err := c.guardTarget(rt, opMutate); err != nil {
		return nil, fileWriteResult{}, err
	}
	if err := c.requireTool(rt, "file_write"); err != nil {
		return nil, fileWriteResult{}, err
	}

	// Non-append writes over an OOB route go through the atomic replace path
	// (temp+rename, mode preserved, symlink-refusing, optional if_match CAS).
	if !args.Append && rt.via != "in_band" {
		if err := c.writeFileAtomic(rt, args.Path, data, args.Mode, args.IfMatch); err != nil {
			return nil, fileWriteResult{}, err
		}
		return nil, fileWriteResult{BytesWritten: len(data), Via: resultVia(rt), Host: rt.host}, nil
	}
	if args.IfMatch != "" {
		return nil, fileWriteResult{}, errors.New("if_match requires an out-of-band route; it is not available for append or in-band writes")
	}

	// Append, and the visible in_band fallback, keep the direct (non-atomic)
	// write.
	switch rt.via {
	case "local":
		path := expandLocal(args.Path)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
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
		caps, _ := c.Mux.CachedCapabilities(rt.ci)
		res, err := c.Mux.ChannelRun(rt.ci, sshmux.WriteScript(args.Path, data, true, args.Mode, caps.Base64Decode()), 60*time.Second)
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
	if err := c.requireTool(rt, "file_edit"); err != nil {
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
	// Automatic staleness protection: verify the file still hashes to what we
	// read before the atomic swap, closing the read->modify->write window. On a
	// host with no sha256 tool we fall back to a plain atomic replace.
	ifMatch := ""
	if c.canSha256(rt) {
		ifMatch = sha256Version(data)
	}
	if err := c.writeFileAtomic(rt, args.Path, updated, "", ifMatch); err != nil {
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
	// Version is a cheap mtime+size token for if_match writes (version_kind
	// "mtime-size"). Weaker than file_read's sha256 — it can miss a same-size,
	// same-mtime change — but needs no hasher on the remote.
	Version     string `json:"version,omitempty"`
	VersionKind string `json:"version_kind,omitempty"`
	Via         string `json:"via"`
	Host        string `json:"host"`
	Warning     string `json:"warning,omitempty"`
}

// setMtimeVersion fills the mtime-size version token from Size/ModifiedUnix.
func (r *fileStatResult) setMtimeVersion() {
	r.Version = fmt.Sprintf("mtime-size:%d:%d", r.ModifiedUnix, r.Size)
	r.VersionKind = "mtime-size"
}

func (c *Core) fileStat(ctx context.Context, req *mcp.CallToolRequest, args fileStatArgs) (*mcp.CallToolResult, fileStatResult, error) {
	if err := validateAbsolutePath(args.Path); err != nil {
		return nil, fileStatResult{}, err
	}
	rt := c.route()
	if rt.via == "in_band" {
		return nil, fileStatResult{}, oobPrimitiveError("file_stat", rt.host)
	}
	if err := c.requireTool(rt, "file_stat"); err != nil {
		return nil, fileStatResult{}, err
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
		res.setMtimeVersion()
		return nil, res, nil
	}

	caps, _ := c.Mux.CachedCapabilities(rt.ci)
	// GNU/BusyBox stat use -c; BSD/macOS use -f. Both are asked for the same
	// fields in the same order (mode, size, mtime, type), so one parse handles
	// all; only the type strings differ (remoteFileType normalizes them).
	var cmd string
	switch {
	case caps.StatC:
		cmd = "stat -c '%a\t%s\t%Y\t%F' -- " + sshmux.Quote(args.Path)
	case caps.StatF:
		cmd = "stat -f '%Lp\t%z\t%m\t%HT' " + sshmux.Quote(args.Path)
	default:
		cmd = "stat -c '%a\t%s\t%Y\t%F' -- " + sshmux.Quote(args.Path)
	}
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
	res.setMtimeVersion()
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
	if err := c.requireTool(rt, "directory_list"); err != nil {
		return nil, directoryListResult{}, err
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

	caps, _ := c.Mux.CachedCapabilities(rt.ci)
	var entries []directoryEntry
	var err error
	if caps.FindPrint && caps.HeadZ {
		entries, err = c.dirListGNU(rt.ci, args.Path, max)
	} else {
		entries, err = c.dirListPortable(rt.ci, args.Path, caps, max)
	}
	if err != nil {
		return nil, directoryListResult{}, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	res.Truncated = len(entries) > max
	if len(entries) > max {
		entries = entries[:max]
	}
	res.Entries = entries
	return nil, res, nil
}

// dirListGNU uses GNU find -printf with NUL fields, which keep names containing
// tabs or newlines unambiguous.
func (c *Core) dirListGNU(ci *sshmux.ConnInfo, path string, max int) ([]directoryEntry, error) {
	fieldLimit := (max + 1) * 4
	cmd := fmt.Sprintf("test -d %s && find -H %s -mindepth 1 -maxdepth 1 -printf '%%f\\0%%y\\0%%s\\0%%T@\\0' | head -z -n %d",
		sshmux.Quote(path), sshmux.Quote(path), fieldLimit)
	out, err := c.channelOutput(ci, cmd, 60*time.Second)
	if err != nil {
		return nil, err
	}
	parts := bytes.Split(out, []byte{0})
	if len(parts) > 0 && len(parts[len(parts)-1]) == 0 {
		parts = parts[:len(parts)-1]
	}
	if len(parts)%4 != 0 {
		return nil, errors.New("unexpected directory listing response")
	}
	var entries []directoryEntry
	for i := 0; i < len(parts); i += 4 {
		size, serr := strconv.ParseInt(string(parts[i+2]), 10, 64)
		mtime, merr := strconv.ParseFloat(string(parts[i+3]), 64)
		if serr != nil || merr != nil {
			return nil, fmt.Errorf("invalid directory entry metadata for %q", parts[i])
		}
		entries = append(entries, directoryEntry{
			Name: string(parts[i]), Type: findFileType(string(parts[i+1])), Size: size, ModifiedUnix: int64(mtime),
		})
	}
	return entries, nil
}

// dirListPortable works without GNU find -printf: it drives `find … -exec stat`
// with the host's stat flavor. Output is one tab-separated line per entry
// (path, type, size, mtime); newline/tab framing is best-effort (names with
// those bytes are ambiguous — the GNU path avoids that).
func (c *Core) dirListPortable(ci *sshmux.ConnInfo, path string, caps sshmux.Capabilities, max int) ([]directoryEntry, error) {
	var statExpr string
	if caps.StatF {
		statExpr = "stat -f '%N\t%HT\t%z\t%m'"
	} else {
		statExpr = "stat -c '%n\t%F\t%s\t%Y'"
	}
	cmd := fmt.Sprintf("test -d %s && find -H %s -mindepth 1 -maxdepth 1 -exec %s {} + | head -n %d",
		sshmux.Quote(path), sshmux.Quote(path), statExpr, max+1)
	out, err := c.channelOutput(ci, cmd, 60*time.Second)
	if err != nil {
		return nil, err
	}
	var entries []directoryEntry
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		f := strings.SplitN(line, "\t", 4)
		if len(f) != 4 {
			continue
		}
		size, _ := strconv.ParseInt(f[2], 10, 64)
		mtime, _ := strconv.ParseInt(f[3], 10, 64)
		entries = append(entries, directoryEntry{
			Name: filepath.Base(f[0]), Type: remoteFileType(f[1]), Size: size, ModifiedUnix: mtime,
		})
	}
	return entries, nil
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
	if err := c.requireTool(rt, "file_upload"); err != nil {
		return nil, transferResult{}, err
	}
	data, err := os.ReadFile(expandLocal(args.LocalPath))
	if err != nil {
		return nil, transferResult{}, err
	}
	if err := c.writeFileAtomic(rt, args.RemotePath, data, "", ""); err != nil {
		return nil, transferResult{}, err
	}
	return nil, transferResult{Bytes: int64(len(data)), Host: rt.host}, nil
}

func (c *Core) fileDownload(ctx context.Context, req *mcp.CallToolRequest, args transferArgs) (*mcp.CallToolResult, transferResult, error) {
	rt := c.route()
	if rt.via != "controlmaster" {
		return nil, transferResult{}, errors.New("no authorized multiplexed SSH channel (session is local, OOB was not enabled before SSH, or the channel is unavailable); use file_read instead")
	}
	if err := c.requireTool(rt, "file_download"); err != nil {
		return nil, transferResult{}, err
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

// Sentinel errors surfaced to the model so it can self-correct.
var (
	errStaleWrite   = errors.New("the file changed since it was read (if_match mismatch); re-read it and retry")
	errSymlinkWrite = errors.New("refusing to write through a symlink; write to the symlink's real target path instead")
	// errNoVersionWrite is distinct from errStaleWrite: the remote couldn't
	// compute the current version (its sha256/stat tool is unavailable, or the
	// file vanished), so the CAS couldn't be checked at all — retrying the same
	// if_match would loop. The caller should re-read (file_read/file_stat picks a
	// verifiable token for this host) or omit if_match.
	errNoVersionWrite = errors.New("could not verify the file's version on this host (its sha256/stat tool is unavailable, or the file no longer exists); re-read it or write without if_match")
)

// sha256Version returns the version token for exactly these bytes.
func sha256Version(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// canSha256 reports whether a sha256 if_match can be *enforced* on rt's write
// path: always locally (aish hashes in Go), and remotely only when the host has
// a sha256 tool to recompute the hash during the compare-and-swap.
func (c *Core) canSha256(rt route) bool {
	switch rt.via {
	case "local":
		return true
	case "controlmaster":
		caps, ok := c.Mux.CachedCapabilities(rt.ci)
		return ok && caps.Hasher != "" && caps.Hasher != "none"
	}
	return false
}

// writeFileAtomic installs data at path atomically (temp file + rename),
// preserving the existing mode (or applying mode), refusing to follow a
// symlink, and — when ifMatch is set — swapping only if the current file still
// matches that version token. Not for append. Returns errStaleWrite /
// errSymlinkWrite on the respective failures.
func (c *Core) writeFileAtomic(rt route, path string, data []byte, mode, ifMatch string) error {
	switch rt.via {
	case "local":
		return atomicWriteLocal(expandLocal(path), data, mode, ifMatch)
	case "controlmaster":
		caps, _ := c.Mux.CachedCapabilities(rt.ci)
		hasher := caps.Hasher
		if strings.HasPrefix(ifMatch, "sha256:") && (hasher == "" || hasher == "none") {
			return fmt.Errorf("cannot verify a sha256 if_match on %s (no sha256 tool); get an mtime-size version from file_stat, or omit if_match", rt.host)
		}
		script := sshmux.AtomicWriteScript(sshmux.WriteRequest{
			Path: path, Data: data, Mode: mode, IfMatch: ifMatch,
			Hasher: hasher, Base64Decode: caps.Base64Decode(),
		})
		res, err := c.Mux.ChannelRun(rt.ci, script, 60*time.Second)
		if err != nil {
			return err
		}
		if res.TimedOut {
			return errors.New("oob channel write timed out")
		}
		switch res.Exit {
		case 0:
			return nil
		case sshmux.WriteExitStale:
			return errStaleWrite
		case sshmux.WriteExitNoVersion:
			return errNoVersionWrite
		case sshmux.WriteExitSymlink:
			return errSymlinkWrite
		default:
			return fmt.Errorf("oob channel write failed (exit %d): %.300s", res.Exit, res.Output)
		}
	default:
		return oobPrimitiveError("write", rt.host)
	}
}

// atomicWriteLocal mirrors AtomicWriteScript for a local-session target: refuse
// symlinks, optional CAS, temp-in-dir + chmod + rename.
func atomicWriteLocal(path string, data []byte, mode, ifMatch string) error {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return errSymlinkWrite
	}
	if ifMatch != "" {
		cur, err := localVersion(path, ifMatch)
		if err != nil {
			return err
		}
		if cur != ifMatch {
			return errStaleWrite
		}
	}
	perm := os.FileMode(0o644)
	if m, err := parseMode(mode); mode != "" && err == nil {
		perm = m.Perm()
	} else if fi, err := os.Stat(path); err == nil {
		perm = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".aishtmp.*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename below succeeds
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// localVersion computes the current version token for path, in the same kind as
// the supplied token (sha256 or mtime-size).
func localVersion(path, tokenKind string) (string, error) {
	switch {
	case strings.HasPrefix(tokenKind, "sha256:"):
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return sha256Version(data), nil
	case strings.HasPrefix(tokenKind, "mtime-size:"):
		fi, err := os.Stat(path)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("mtime-size:%d:%d", fi.ModTime().Unix(), fi.Size()), nil
	}
	return "", errors.New("unsupported if_match token; use a version from file_read or file_stat")
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

// remoteFileType normalizes the type string from GNU stat %F ("regular file",
// "symbolic link", …) or BSD stat %HT ("Regular File", "Symbolic Link", …).
func remoteFileType(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "regular file", "regular empty file":
		return "file"
	case "directory":
		return "directory"
	case "symbolic link":
		return "symlink"
	case "fifo", "fifo file":
		return "fifo"
	case "socket":
		return "socket"
	case "character special file", "character device":
		return "char_device"
	case "block special file", "block device":
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
