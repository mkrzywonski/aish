package aishwnd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ai-ssh/internal/aishwinwire"
)

// These four mirror aish's own file_read/file_write/file_stat/directory_list
// schemas (internal/mcpserver/tools_remote.go) minus the SessionArg routing
// field — same reasoning as runCommandArgs in run_command.go. file_edit/file_patch (which
// build on readRemoteFile/writeRemoteFile below) live in edit.go.
// file_grep/file_search/file_upload/file_download remain later-stage work
// per the plan doc's tool matrix.

type fileReadArgs struct {
	Path        string `json:"path" jsonschema:"absolute path on the Windows host"`
	MaxBytes    int    `json:"max_bytes,omitempty" jsonschema:"cap returned content (default 262144)"`
	Offset      int64  `json:"offset,omitempty" jsonschema:"byte offset to start reading from"`
	LineNumbers bool   `json:"line_numbers,omitempty" jsonschema:"also return numbered_content (line-numbered, from offset 0 only); content stays raw for file_edit"`
}

type fileReadResult struct {
	Content         string `json:"content"`
	Encoding        string `json:"encoding"` // utf8 | base64
	Eof             bool   `json:"eof"`
	NumberedContent string `json:"numbered_content,omitempty"`
	// Version is a whole-file token (only set when the entire file was read
	// in one call); pass it as file_write's if_match to write only if the
	// file hasn't changed since.
	Version     string `json:"version,omitempty"`
	VersionKind string `json:"version_kind,omitempty"`
	Via         string `json:"via"`
	Host        string `json:"host"`
}

type fileWriteArgs struct {
	Path     string `json:"path"`
	Content  string `json:"content"`
	Encoding string `json:"encoding,omitempty" jsonschema:"utf8 (default) or base64"`
	Append   bool   `json:"append,omitempty"`
	Mode     string `json:"mode,omitempty" jsonschema:"octal file mode to set, e.g. 0644 -- best-effort on Windows, which has no POSIX permission bits; only the read-only attribute is affected"`
	IfMatch  string `json:"if_match,omitempty" jsonschema:"only write if the file's current version still equals this token (from a prior file_read or file_stat); fails if the file changed. Not valid with append."`
}

type fileWriteResult struct {
	BytesWritten int    `json:"bytes_written"`
	Via          string `json:"via"`
	Host         string `json:"host"`
}

type fileStatArgs struct {
	Path string `json:"path" jsonschema:"absolute path on the Windows host"`
}

type fileStatResult struct {
	Path         string `json:"path"`
	Type         string `json:"type"`
	Size         int64  `json:"size"`
	Mode         string `json:"mode"`
	ModifiedUnix int64  `json:"modified_unix"`
	Modified     string `json:"modified"` // RFC 3339, the same instant a human can read
	// Version is a cheap mtime+size token for if_match writes (version_kind
	// "mtime-size"). Weaker than file_read's sha256 -- it can miss a
	// same-size, same-mtime change -- but avoids reading the whole file.
	Version     string `json:"version,omitempty"`
	VersionKind string `json:"version_kind,omitempty"`
	Via         string `json:"via"`
	Host        string `json:"host"`
}

type directoryListArgs struct {
	Path       string `json:"path" jsonschema:"absolute directory path on the Windows host"`
	MaxEntries int    `json:"max_entries,omitempty" jsonschema:"maximum entries to return (default 1000, maximum 10000)"`
}

type directoryEntry struct {
	Name         string `json:"name"`
	Type         string `json:"type"`
	Size         int64  `json:"size"`
	ModifiedUnix int64  `json:"modified_unix"`
	Modified     string `json:"modified"` // RFC 3339, the same instant a human can read
}

type directoryListResult struct {
	Entries   []directoryEntry `json:"entries"`
	Truncated bool             `json:"truncated"`
	Via       string           `json:"via"`
	Host      string           `json:"host"`
}

func registerFileTools(s *mcp.Server, sess *aishwndSession) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "file_read",
		Annotations: readOnlyTool("Read file on Windows host"),
		Description: "Read a file from the Windows host. Non-UTF-8 content is returned base64 (see encoding).",
	}, sess.fileRead)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "file_write",
		Annotations: mutatingTool("Write file on Windows host", true, false),
		Description: "Write (or append to) a file on the Windows host. Content is UTF-8, or base64 with " +
			"encoding=base64. Non-append writes are atomic (temp file + rename) and refuse to follow a symlink.",
	}, sess.fileWrite)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "file_stat",
		Annotations: readOnlyTool("Inspect path on Windows host"),
		Description: "Inspect an absolute path on the Windows host. Returns its type, size, permissions, and " +
			"modification time without following a symbolic link.",
	}, sess.fileStat)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "directory_list",
		Annotations: readOnlyTool("List directory on Windows host"),
		Description: "List direct children of an absolute directory on the Windows host — the Windows " +
			"equivalent of your Bash `ls -la`. Sorted by name, with type, size, and modification time. " +
			"Symlinks are reported as symlinks and are not followed; hidden files are included.",
	}, sess.directoryList)
}

func (s *aishwndSession) fileRead(ctx context.Context, req *mcp.CallToolRequest, args fileReadArgs) (*mcp.CallToolResult, fileReadResult, error) {
	if args.Path == "" {
		return nil, fileReadResult{}, errors.New("path must not be empty")
	}
	raw, eof, err := s.readRemoteFile(args.Path, args.Offset, args.MaxBytes)
	if err != nil {
		return nil, fileReadResult{}, err
	}

	out := fileReadResult{Eof: eof, Via: "aishwin", Host: s.displayHost()}
	if args.Offset == 0 && eof {
		// The whole file is in hand: a sha256 over these exact bytes is a
		// TOCTOU-correct version token for a later if_match write.
		out.Version, out.VersionKind = aishwinwire.SHA256Version(raw), "sha256"
	}
	if utf8.Valid(raw) {
		out.Content, out.Encoding = string(raw), "utf8"
		if args.LineNumbers && args.Offset == 0 {
			out.NumberedContent = numberLines(raw)
		}
	} else {
		out.Content, out.Encoding = base64.StdEncoding.EncodeToString(raw), "base64"
	}
	return nil, out, nil
}

// readRemoteFile sends a file_read wire request and returns the decoded raw
// bytes. maxBytes<=0 uses the Windows side's own default (see
// defaultMaxFileRead in cmd/aishwin/files_dispatch.go). Shared by fileRead,
// fileEdit, and filePatch — each needs the same "get the file's current raw
// bytes" step before doing something different with them.
func (s *aishwndSession) readRemoteFile(path string, offset int64, maxBytes int) (data []byte, eof bool, err error) {
	req, err := json.Marshal(aishwinwire.FileReadData{Path: path, MaxBytes: maxBytes, Offset: offset})
	if err != nil {
		return nil, false, err
	}
	res, err := s.roundTrip("file_read", req, 30*time.Second)
	if err != nil {
		return nil, false, err
	}
	var wireRes aishwinwire.FileReadResultData
	if err := json.Unmarshal(res, &wireRes); err != nil {
		return nil, false, fmt.Errorf("malformed file_read result from the Windows peer: %w", err)
	}
	if wireRes.Error != "" {
		return nil, false, errors.New(wireRes.Error)
	}
	raw, err := base64.StdEncoding.DecodeString(wireRes.Content)
	if err != nil {
		return nil, false, fmt.Errorf("malformed content from the Windows peer: %w", err)
	}
	return raw, wireRes.Eof, nil
}

func (s *aishwndSession) fileWrite(ctx context.Context, req *mcp.CallToolRequest, args fileWriteArgs) (*mcp.CallToolResult, fileWriteResult, error) {
	if args.Path == "" {
		return nil, fileWriteResult{}, errors.New("path must not be empty")
	}
	content := []byte(args.Content)
	if args.Encoding == "base64" {
		decoded, err := base64.StdEncoding.DecodeString(args.Content)
		if err != nil {
			return nil, fileWriteResult{}, err
		}
		content = decoded
	}
	if args.IfMatch != "" && args.Append {
		return nil, fileWriteResult{}, errors.New("if_match cannot be combined with append")
	}

	n, err := s.writeRemoteFile(args.Path, content, args.Mode, args.IfMatch, args.Append)
	if err != nil {
		return nil, fileWriteResult{}, err
	}
	return nil, fileWriteResult{BytesWritten: n, Via: "aishwin", Host: s.displayHost()}, nil
}

// writeRemoteFile sends a file_write wire request. Shared by fileWrite,
// fileEdit, and filePatch — the latter two derive ifMatch automatically
// (from what they just read) rather than taking it from the caller.
func (s *aishwndSession) writeRemoteFile(path string, data []byte, mode, ifMatch string, append bool) (bytesWritten int, err error) {
	req, err := json.Marshal(aishwinwire.FileWriteData{
		Path:    path,
		Content: base64.StdEncoding.EncodeToString(data),
		Append:  append,
		Mode:    mode,
		IfMatch: ifMatch,
	})
	if err != nil {
		return 0, err
	}
	res, err := s.roundTrip("file_write", req, 30*time.Second)
	if err != nil {
		return 0, err
	}
	var wireRes aishwinwire.FileWriteResultData
	if err := json.Unmarshal(res, &wireRes); err != nil {
		return 0, fmt.Errorf("malformed file_write result from the Windows peer: %w", err)
	}
	if wireRes.Error != "" {
		return 0, errors.New(wireRes.Error)
	}
	return wireRes.BytesWritten, nil
}

func (s *aishwndSession) fileStat(ctx context.Context, req *mcp.CallToolRequest, args fileStatArgs) (*mcp.CallToolResult, fileStatResult, error) {
	if args.Path == "" {
		return nil, fileStatResult{}, errors.New("path must not be empty")
	}
	data, err := json.Marshal(aishwinwire.FileStatData{Path: args.Path})
	if err != nil {
		return nil, fileStatResult{}, err
	}

	res, err := s.roundTrip("file_stat", data, 20*time.Second)
	if err != nil {
		return nil, fileStatResult{}, err
	}
	var wireRes aishwinwire.FileStatResultData
	if err := json.Unmarshal(res, &wireRes); err != nil {
		return nil, fileStatResult{}, fmt.Errorf("malformed file_stat result from the Windows peer: %w", err)
	}
	if wireRes.Error != "" {
		return nil, fileStatResult{}, errors.New(wireRes.Error)
	}

	out := fileStatResult{
		Path: args.Path, Type: wireRes.Type, Size: wireRes.Size, Mode: wireRes.Mode,
		ModifiedUnix: wireRes.ModifiedUnix, Modified: rfc3339(wireRes.ModifiedUnix), Via: "aishwin", Host: s.displayHost(),
	}
	out.Version = fmt.Sprintf("mtime-size:%d:%d", out.ModifiedUnix, out.Size)
	out.VersionKind = "mtime-size"
	return nil, out, nil
}

func (s *aishwndSession) directoryList(ctx context.Context, req *mcp.CallToolRequest, args directoryListArgs) (*mcp.CallToolResult, directoryListResult, error) {
	if args.Path == "" {
		return nil, directoryListResult{}, errors.New("path must not be empty")
	}
	max := args.MaxEntries
	if max <= 0 {
		max = 1000
	}
	if max > 10000 {
		return nil, directoryListResult{}, errors.New("max_entries must not exceed 10000")
	}

	data, err := json.Marshal(aishwinwire.DirectoryListData{Path: args.Path, MaxEntries: max})
	if err != nil {
		return nil, directoryListResult{}, err
	}

	res, err := s.roundTrip("directory_list", data, 30*time.Second)
	if err != nil {
		return nil, directoryListResult{}, err
	}
	var wireRes aishwinwire.DirectoryListResultData
	if err := json.Unmarshal(res, &wireRes); err != nil {
		return nil, directoryListResult{}, fmt.Errorf("malformed directory_list result from the Windows peer: %w", err)
	}
	if wireRes.Error != "" {
		return nil, directoryListResult{}, errors.New(wireRes.Error)
	}

	entries := make([]directoryEntry, len(wireRes.Entries))
	for i, e := range wireRes.Entries {
		entries[i] = directoryEntry{Name: e.Name, Type: e.Type, Size: e.Size, ModifiedUnix: e.ModifiedUnix, Modified: rfc3339(e.ModifiedUnix)}
	}
	return nil, directoryListResult{Entries: entries, Truncated: wireRes.Truncated, Via: "aishwin", Host: s.displayHost()}, nil
}

// numberLines renders content with 1-based line numbers (cat -n style),
// mirroring aish's own helper of the same name (internal/mcpserver/
// tools_remote.go) -- kept separate from raw content so line numbers never
// leak into an edit's old_text.
func numberLines(data []byte) string {
	lines := strings.Split(string(data), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	var b strings.Builder
	for i, line := range lines {
		fmt.Fprintf(&b, "%6d\t%s\n", i+1, line)
	}
	return b.String()
}

// rfc3339 renders a Unix timestamp the way a reader can use directly. The
// epoch integer stays, since callers compare and sort on it, but returning
// only that forced anyone wanting a date to convert it by hand — and to guess
// a timezone. The activity log already reports RFC 3339, so this is also two
// tools in the same product agreeing on how to say the same thing.
func rfc3339(unix int64) string {
	if unix == 0 {
		return ""
	}
	return time.Unix(unix, 0).Format(time.RFC3339)
}
