package aishwnd

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// maxTransferBytes bounds file_upload/file_download. Unlike file_read/
// file_write (which chunk via offset/max_bytes for arbitrarily large
// files), these transfer a whole file as one operation, matching aish's
// own file_upload/file_download (a single os.ReadFile / single write, no
// explicit cap beyond memory/timeout — internal/mcpserver/tools_remote.go's
// fileUpload reads the whole local file, fileDownload's channel route
// writes the whole decoded result in one os.WriteFile, neither chunked).
// This cap exists because the wire protocol needs SOME bound for a
// single-frame transfer; internal/aishwinwire.MaxFrameLine is sized to match.
const maxTransferBytes = 32 << 20

// transferArgs/transferResult mirror aish's own schema
// (internal/mcpserver/tools_remote.go) minus SessionArg.
type transferArgs struct {
	LocalPath  string `json:"local_path" jsonschema:"absolute path on the Linux/WSL machine (where this MCP server runs)"`
	RemotePath string `json:"remote_path" jsonschema:"absolute path on the Windows host"`
}

type transferResult struct {
	Bytes int64  `json:"bytes"`
	Via   string `json:"via"`
	Host  string `json:"host"`
}

func registerTransferTools(s *mcp.Server, sess *aishwndSession) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "file_upload",
		Annotations: mutatingTool("Upload file to Windows host", true, false),
		Description: fmt.Sprintf("Copy a file from the Linux/WSL machine (where this MCP server runs) to the "+
			"Windows host. Whole-file, one operation — not for files larger than %d bytes; chunk larger transfers "+
			"with file_read/file_write instead.", maxTransferBytes),
	}, sess.fileUpload)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "file_download",
		Annotations: mutatingTool("Download file from Windows host", true, false),
		Description: fmt.Sprintf("Copy a file from the Windows host to the Linux/WSL machine (where this MCP "+
			"server runs). Whole-file, one operation — not for files larger than %d bytes; chunk larger transfers "+
			"with file_read/file_write instead.", maxTransferBytes),
	}, sess.fileDownload)
}

func (s *aishwndSession) fileUpload(ctx context.Context, req *mcp.CallToolRequest, args transferArgs) (*mcp.CallToolResult, transferResult, error) {
	if args.LocalPath == "" || args.RemotePath == "" {
		return nil, transferResult{}, errors.New("local_path and remote_path must not be empty")
	}
	data, err := os.ReadFile(args.LocalPath)
	if err != nil {
		return nil, transferResult{}, err
	}
	if len(data) > maxTransferBytes {
		return nil, transferResult{}, fmt.Errorf("local file is %d bytes, exceeding the file_upload limit of %d; chunk it with file_read/file_write instead", len(data), maxTransferBytes)
	}
	// No if_match: file_upload is a fresh write, not a compare-and-swap,
	// matching aish's own writeFileAtomic(ctx, rt, path, data, "", "") call
	// for this same tool.
	n, err := s.writeRemoteFile(args.RemotePath, data, "", "", false)
	if err != nil {
		return nil, transferResult{}, err
	}
	return nil, transferResult{Bytes: int64(n), Via: "aishwin", Host: s.displayHost()}, nil
}

func (s *aishwndSession) fileDownload(ctx context.Context, req *mcp.CallToolRequest, args transferArgs) (*mcp.CallToolResult, transferResult, error) {
	if args.LocalPath == "" || args.RemotePath == "" {
		return nil, transferResult{}, errors.New("local_path and remote_path must not be empty")
	}
	data, eof, err := s.readRemoteFile(args.RemotePath, 0, maxTransferBytes)
	if err != nil {
		return nil, transferResult{}, err
	}
	if !eof {
		return nil, transferResult{}, fmt.Errorf("remote file exceeds the file_download limit of %d bytes; chunk it with file_read/file_write instead", maxTransferBytes)
	}
	// Not atomic (plain os.WriteFile), matching aish's own channel-route
	// file_download — only its SFTP route (which aishwin has no equivalent
	// of) writes via temp+rename.
	if err := os.WriteFile(args.LocalPath, data, 0o644); err != nil {
		return nil, transferResult{}, err
	}
	return nil, transferResult{Bytes: int64(len(data)), Via: "aishwin", Host: s.displayHost()}, nil
}
