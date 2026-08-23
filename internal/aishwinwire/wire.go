// Package aishwinwire is the private wire protocol between cmd/aishwin
// (Windows) and cmd/aishwnd (Linux): newline-delimited JSON over the child
// process's stdio pipes, type-discriminated, correlated by request id.
// Shared by both binaries so the frame shapes can't drift between them —
// unlike the duplicated auth state machine (see internal/aishwnd/auth.go),
// which is business logic private to aishwnd's own unexported types, this is
// pure wire format both sides must agree on byte-for-byte.
package aishwinwire

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// ProtoVersion guards the hello handshake. Bumping it lets either side
// refuse a mismatched peer with a clear error instead of misparsing frames.
const ProtoVersion = 1

// MaxFrameLine bounds a single wire frame. Sized for file_upload/
// file_download (internal/aishwnd's maxTransferBytes, 32MiB), which -- like
// aish's own file_upload/file_download -- transfer a whole file as one
// atomic operation rather than chunking it, so the base64-encoded content
// (~4/3 inflation) of the largest allowed transfer must fit in one frame
// with headroom for JSON overhead. bufio.Scanner grows its buffer on demand
// up to this limit, so ordinary small frames (hello/prompt/notify/exec)
// cost nothing extra.
const MaxFrameLine = 48 << 20

// Frame is the envelope for every message on the wire: one JSON object per
// line. Data is dispatched by Type. Frames with a non-empty ID participate
// in request/response correlation (see Conn.Await); one-way types (hello,
// notify) leave ID empty.
type Frame struct {
	Type string          `json:"type"`
	ID   string          `json:"id,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

type HelloData struct {
	Proto int    `json:"proto"`
	Name  string `json:"name,omitempty"`
	Shell string `json:"shell,omitempty"`
}

// HelloAckData is aishwnd's one reply to the hello frame, sent once the
// session directory/socket are up, before it starts serving MCP or reading
// further wire frames. Lets aishwin display "connected as session <id>"
// immediately and answer the menu's version command without a separate
// round trip.
type HelloAckData struct {
	SessionID string `json:"session_id"`
	Name      string `json:"name,omitempty"`
	Version   string `json:"version"`
}

// RenameData requests renaming the live session, sent by aishwin (from its
// console menu) to aishwnd -- the reverse direction from exec/file_*, which
// aishwnd sends to aishwin. Conn's Send/Await are symmetric, so no separate
// mechanism is needed for a request originating on this side.
type RenameData struct {
	Name string `json:"name"`
}

type RenameResultData struct {
	Error string `json:"error,omitempty"`
}

// ListClientsData requests the MCP clients currently connected to
// aishwnd's Unix socket, sent by aishwin (from its Session > Clients...
// dialog) to aishwnd -- the reverse direction from exec/file_*, same as
// RenameData. No fields: the Windows side always wants the full current
// list.
type ListClientsData struct{}

// ClientData is one connected MCP client, mirroring the shape of
// internal/mcpserver's ConnectedClient (aishwnd can't import that package
// -- see internal/aishwnd/auth.go's own connAuth) minus the kernel-verified
// peer, which has no equivalent here (there's no local socket to check
// SO_PEERCRED on; every "connection" is relayed through this same wire
// link's single Unix socket listener).
type ClientData struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Version     string `json:"version,omitempty"`
	Description string `json:"description,omitempty"`
	State       string `json:"state"`
	SinceUnix   int64  `json:"since_unix,omitempty"`
}

type ListClientsResultData struct {
	Clients []ClientData `json:"clients,omitempty"`
}

// DisconnectClientData requests that aishwnd close one specific client
// connection (identified by ClientData.ID) and forget its grant, so a
// pooled client can't silently keep reusing it -- mirroring the intent of
// internal/mcpserver/connauth.go's Revoke, but for a single connection
// rather than every one at once.
type DisconnectClientData struct {
	ID string `json:"id"`
}

type DisconnectClientResultData struct {
	Error string `json:"error,omitempty"`
}

// CaptureScreenData requests a screenshot from the Windows peer, sent by
// aishwnd (from the AI's capture_screen tool call) to aishwin -- the same
// direction as exec/file_*. Mode is "" or "window" (the aishwin window
// itself, via PrintWindow) or "full"/"screen" (the whole desktop,
// including the taskbar, via a screen-DC capture) -- the latter requires
// a one-time human consent prompt on the Windows console the first time
// it's used each session (aishwin's own fullScreenCaptureAllowed).
type CaptureScreenData struct {
	Mode string `json:"mode,omitempty"`
}

// CaptureScreenResultData answers a CaptureScreenData request. PNG is
// base64-encoded image bytes on success; the aishwnd side decodes it back
// to raw bytes for the MCP tool result's ImageContent, since JSON has no
// native binary type.
type CaptureScreenResultData struct {
	PNG   string `json:"png,omitempty"`
	Error string `json:"error,omitempty"`
}

type PromptData struct {
	Question       string `json:"question"`
	Kind           string `json:"kind"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

type PromptAnswerData struct {
	// Answer is "y"/"n", or empty if the human didn't respond in time.
	Answer string `json:"answer,omitempty"`
}

type NotifyData struct {
	Text string `json:"text"`
}

// ExecData requests a command run on the Windows peer, mirroring aish's own
// exec tool args (internal/mcpserver/tools_remote.go's execArgs) minus the
// SessionArg routing field, which aishwnd doesn't implement cross-session
// forwarding for.
type ExecData struct {
	Command    string `json:"command"`
	Cwd        string `json:"cwd,omitempty"`
	Background bool   `json:"background,omitempty"`
	TimeoutMs  int    `json:"timeout_ms,omitempty"`
}

// ExecResultData answers an ExecData request, mirroring aish's execResult
// shape (minus Via/Host, which aishwnd fills in itself since they're
// constant for this transport).
type ExecResultData struct {
	Output   string `json:"output,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
	TaskID   string `json:"task_id,omitempty"`
	TimedOut bool   `json:"timed_out,omitempty"`
	Error    string `json:"error,omitempty"`
}

// ExecPollData polls a background task started by an ExecData with
// Background: true, mirroring aish's execStatusArgs.
type ExecPollData struct {
	TaskID string `json:"task_id"`
	Cursor int64  `json:"cursor,omitempty"`
}

// ExecPollResultData answers an ExecPollData poll, mirroring aish's
// execStatusResult shape.
type ExecPollResultData struct {
	Running    bool   `json:"running"`
	Output     string `json:"output,omitempty"`
	NextCursor int64  `json:"next_cursor"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	Error      string `json:"error,omitempty"`
}

// FileReadData requests file content from the Windows peer.
type FileReadData struct {
	Path     string `json:"path"`
	MaxBytes int    `json:"max_bytes,omitempty"`
	Offset   int64  `json:"offset,omitempty"`
}

// FileReadResultData answers a FileReadData request. Content is always
// base64 here (binary-safe, one wire encoding) regardless of what the AI
// asked for — aishwnd decides utf8-vs-base64 presentation after decoding,
// exactly like aish's own fileRead does for its "local" route.
type FileReadResultData struct {
	Content string `json:"content,omitempty"`
	Eof     bool   `json:"eof"`
	Error   string `json:"error,omitempty"`
}

// FileWriteData requests an atomic (or, if Append, non-atomic appending)
// write on the Windows peer. Content is always base64. IfMatch, when set,
// carries the full version token (e.g. "sha256:..." or "mtime-size:...")
// from a prior read/stat — the Windows side must check-and-write it
// atomically, since that's the only place the file's current bytes and the
// write can be observed together without a TOCTOU gap.
type FileWriteData struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Append  bool   `json:"append,omitempty"`
	Mode    string `json:"mode,omitempty"`
	IfMatch string `json:"if_match,omitempty"`
}

type FileWriteResultData struct {
	BytesWritten int    `json:"bytes_written"`
	Error        string `json:"error,omitempty"`
}

// FileStatData requests metadata for path on the Windows peer.
type FileStatData struct {
	Path string `json:"path"`
}

type FileStatResultData struct {
	Type         string `json:"type,omitempty"`
	Size         int64  `json:"size"`
	Mode         string `json:"mode,omitempty"`
	ModifiedUnix int64  `json:"modified_unix"`
	Error        string `json:"error,omitempty"`
}

// DirectoryListData requests directory entries from the Windows peer.
type DirectoryListData struct {
	Path       string `json:"path"`
	MaxEntries int    `json:"max_entries,omitempty"`
}

type DirEntryData struct {
	Name         string `json:"name"`
	Type         string `json:"type"`
	Size         int64  `json:"size"`
	ModifiedUnix int64  `json:"modified_unix"`
}

type DirectoryListResultData struct {
	Entries   []DirEntryData `json:"entries,omitempty"`
	Truncated bool           `json:"truncated"`
	Error     string         `json:"error,omitempty"`
}

// GrepData requests a content search under Path on the Windows peer.
type GrepData struct {
	Path       string `json:"path"`
	Pattern    string `json:"pattern"`
	Include    string `json:"include,omitempty"`
	IgnoreCase bool   `json:"ignore_case,omitempty"`
	MaxResults int    `json:"max_results,omitempty"`
}

type GrepMatchData struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

type GrepResultData struct {
	Matches   []GrepMatchData `json:"matches,omitempty"`
	Truncated bool            `json:"truncated"`
	Error     string          `json:"error,omitempty"`
}

// SearchData requests a filename search under Path on the Windows peer.
type SearchData struct {
	Path       string `json:"path"`
	Name       string `json:"name,omitempty"`
	Type       string `json:"type,omitempty"`
	MaxResults int    `json:"max_results,omitempty"`
}

type SearchResultData struct {
	Paths     []string `json:"paths,omitempty"`
	Truncated bool     `json:"truncated"`
	Error     string   `json:"error,omitempty"`
}

// Conn is one side of the private wire protocol: JSON-line framing over a
// reader/writer pair (normally a child process's stdio pipes), with
// request/response correlation by ID for frame types that expect an answer
// (currently just prompt/prompt_answer).
type Conn struct {
	sc *bufio.Scanner

	wmu sync.Mutex
	w   *bufio.Writer

	pendingMu sync.Mutex
	pending   map[string]chan Frame
}

func NewConn(r io.Reader, w io.Writer) *Conn {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), MaxFrameLine)
	return &Conn{
		sc:      sc,
		w:       bufio.NewWriter(w),
		pending: map[string]chan Frame{},
	}
}

func (c *Conn) Send(f Frame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if _, err := c.w.Write(b); err != nil {
		return err
	}
	if err := c.w.WriteByte('\n'); err != nil {
		return err
	}
	return c.w.Flush()
}

// Await registers a pending response channel for id; ReadLoop delivers the
// matching frame there when it arrives instead of passing it to fn.
func (c *Conn) Await(id string) chan Frame {
	ch := make(chan Frame, 1)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()
	return ch
}

func (c *Conn) CancelAwait(id string) {
	c.pendingMu.Lock()
	delete(c.pending, id)
	c.pendingMu.Unlock()
}

// ReadOne reads a single frame synchronously. Used once for the hello
// handshake before ReadLoop takes over; not safe to call concurrently with
// ReadLoop since both pull from the same underlying scanner.
func (c *Conn) ReadOne() (Frame, error) {
	if !c.sc.Scan() {
		if err := c.sc.Err(); err != nil {
			return Frame{}, err
		}
		return Frame{}, io.EOF
	}
	var f Frame
	if err := json.Unmarshal(c.sc.Bytes(), &f); err != nil {
		return Frame{}, fmt.Errorf("malformed frame: %w", err)
	}
	return f, nil
}

// ReadLoop dispatches frames until the connection closes or errors.
// Malformed lines are skipped rather than killing the link. Frames whose ID
// matches a pending Await() are delivered there instead of to fn.
func (c *Conn) ReadLoop(fn func(Frame)) error {
	for c.sc.Scan() {
		var f Frame
		if err := json.Unmarshal(c.sc.Bytes(), &f); err != nil {
			continue
		}
		if f.ID != "" {
			c.pendingMu.Lock()
			ch, ok := c.pending[f.ID]
			if ok {
				delete(c.pending, f.ID)
			}
			c.pendingMu.Unlock()
			if ok {
				ch <- f
				continue
			}
		}
		fn(f)
	}
	return c.sc.Err()
}

// ReadHello reads and validates the one-shot hello frame that must be the
// first thing sent on a fresh Conn.
func ReadHello(c *Conn) (HelloData, error) {
	f, err := c.ReadOne()
	if err != nil {
		return HelloData{}, err
	}
	if f.Type != "hello" {
		return HelloData{}, fmt.Errorf("expected hello frame, got %q", f.Type)
	}
	var h HelloData
	if err := json.Unmarshal(f.Data, &h); err != nil {
		return HelloData{}, fmt.Errorf("malformed hello: %w", err)
	}
	if h.Proto != ProtoVersion {
		return HelloData{}, fmt.Errorf("unsupported protocol version %d (this build speaks %d)", h.Proto, ProtoVersion)
	}
	return h, nil
}
