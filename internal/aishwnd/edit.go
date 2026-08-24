package aishwnd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ai-ssh/internal/aishwinwire"
)

// maxFileEdit bounds file_edit/file_patch, matching aish's own limit
// (internal/mcpserver/tools_remote.go's maxFileEdit) — exact-match edits
// and diff application both stay bounded rather than streaming.
const maxFileEdit = 1 << 20

// fileEditArgs/fileEditResult and filePatchArgs/filePatchResult mirror
// aish's own schemas (internal/mcpserver/tools_remote.go's fileEditArgs,
// internal/mcpserver/patch.go's filePatchArgs) minus SessionArg, same
// reasoning as files.go's four tools.

type fileEditArgs struct {
	Path       string `json:"path" jsonschema:"absolute path on the Windows host"`
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

type filePatchArgs struct {
	Path    string `json:"path" jsonschema:"absolute path on the Windows host"`
	Patch   string `json:"patch" jsonschema:"a unified diff (@@ hunks) describing the change; context lines start with a space, removals with -, additions with +"`
	IfMatch string `json:"if_match,omitempty" jsonschema:"only apply if the file's current version still equals this token (from a prior file_read or file_stat)"`
}

type filePatchResult struct {
	HunksApplied int    `json:"hunks_applied"`
	BytesWritten int    `json:"bytes_written"`
	Via          string `json:"via"`
	Host         string `json:"host"`
}

func registerEditTools(s *mcp.Server, sess *aishwndSession) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "file_edit",
		Annotations: mutatingTool("Edit file on Windows host", true, false),
		Description: "Edit a UTF-8 text file on the Windows host by replacing exact text. Fails when old_text " +
			"is absent or occurs more than once unless replace_all=true. Automatically guards against a " +
			"read-modify-write race: the write only applies if the file is still exactly what was just read. " +
			"That guard is unconditional here; on the shared-terminal backend it holds only when the route can " +
			"verify SHA-256, so do not carry this guarantee across to a session on the other backend.",
	}, sess.fileEdit)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "file_patch",
		Annotations: mutatingTool("Patch file on Windows host", true, false),
		Description: "Apply a unified diff to a UTF-8 text file on the Windows host, one file per call. Hunks are " +
			"applied here (no remote patch tool needed) and written back atomically. Use file_edit for a single " +
			"exact-text replacement; use file_patch for multi-hunk changes. Optionally pass if_match (a version " +
			"from file_read/file_stat) to apply only if the file is unchanged; otherwise staleness is checked " +
			"automatically from what was just read.",
	}, sess.filePatch)
}

func (s *aishwndSession) fileEdit(ctx context.Context, req *mcp.CallToolRequest, args fileEditArgs) (*mcp.CallToolResult, fileEditResult, error) {
	if args.Path == "" {
		return nil, fileEditResult{}, errors.New("path must not be empty")
	}
	if args.OldText == "" {
		return nil, fileEditResult{}, errors.New("old_text must not be empty")
	}

	data, eof, err := s.readRemoteFile(args.Path, 0, maxFileEdit)
	if err != nil {
		return nil, fileEditResult{}, err
	}
	if !eof {
		return nil, fileEditResult{}, fmt.Errorf("file exceeds the file_edit limit of %d bytes; edit it in smaller pieces via file_write", maxFileEdit)
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

	// Automatic staleness protection: the write only applies if the file
	// still hashes to what was just read, closing the read-modify-write race.
	written, err := s.writeRemoteFile(args.Path, updated, "", aishwinwire.SHA256Version(data), false)
	if err != nil {
		return nil, fileEditResult{}, err
	}
	if !args.ReplaceAll {
		count = 1
	}
	return nil, fileEditResult{Replacements: count, BytesWritten: written, Via: "aishwin", Host: s.displayHost()}, nil
}

func (s *aishwndSession) filePatch(ctx context.Context, req *mcp.CallToolRequest, args filePatchArgs) (*mcp.CallToolResult, filePatchResult, error) {
	if args.Path == "" {
		return nil, filePatchResult{}, errors.New("path must not be empty")
	}
	if strings.TrimSpace(args.Patch) == "" {
		return nil, filePatchResult{}, errors.New("patch must not be empty")
	}
	hunks, err := parseUnifiedDiff(args.Patch)
	if err != nil {
		return nil, filePatchResult{}, err
	}

	data, eof, err := s.readRemoteFile(args.Path, 0, maxFileEdit)
	if err != nil {
		return nil, filePatchResult{}, err
	}
	if !eof {
		return nil, filePatchResult{}, fmt.Errorf("file exceeds the file_patch limit of %d bytes", maxFileEdit)
	}
	if !utf8.Valid(data) {
		return nil, filePatchResult{}, errors.New("file_patch requires a UTF-8 text file")
	}
	updated, err := applyUnifiedDiff(data, hunks)
	if err != nil {
		return nil, filePatchResult{}, err
	}
	if len(updated) > maxFileEdit {
		return nil, filePatchResult{}, fmt.Errorf("patched file would exceed the file_patch limit of %d bytes", maxFileEdit)
	}

	// Prefer an explicit if_match; otherwise derive one automatically from
	// what was just read (same TOCTOU guard as file_edit).
	ifMatch := args.IfMatch
	if ifMatch == "" {
		ifMatch = aishwinwire.SHA256Version(data)
	}
	written, err := s.writeRemoteFile(args.Path, updated, "", ifMatch, false)
	if err != nil {
		return nil, filePatchResult{}, err
	}
	return nil, filePatchResult{HunksApplied: len(hunks), BytesWritten: written, Via: "aishwin", Host: s.displayHost()}, nil
}
