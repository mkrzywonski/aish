package aishwnd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	// Sha256 lets one call answer "did it arrive intact". Without it,
	// confirming a transfer meant a second round trip to file_read or
	// file_stat, even though file_read already computes exactly this.
	Sha256 string `json:"sha256,omitempty"`
}

func registerTransferTools(s *mcp.Server, sess *aishwndSession) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "file_upload",
		Annotations: mutatingTool("Upload file to Windows host", true, false),
		Description: fmt.Sprintf("Copy a file from the Linux/WSL machine (where this MCP server runs) to the "+
			"Windows host — the direction of `scp local_path remote:remote_path`, so local_path is always the "+
			"Linux side and remote_path always the Windows side. Whole-file, one operation — not for files "+
			"larger than %d bytes; chunk larger transfers with file_read/file_write instead. The result carries "+
			"the content sha256, so \"did it arrive intact\" is answered by this call rather than a follow-up read.", maxTransferBytes),
	}, sess.fileUpload)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "file_download",
		Annotations: mutatingTool("Download file from Windows host", true, false),
		Description: fmt.Sprintf("Copy a file from the Windows host to the Linux/WSL machine (where this MCP "+
			"server runs) — the direction of `scp remote:remote_path local_path`, so remote_path is always the "+
			"Windows side and local_path always the Linux side. Whole-file, one operation — not for files larger "+
			"than %d bytes; chunk larger transfers with file_read/file_write instead. The result carries the "+
			"content sha256, so \"did it arrive intact\" is answered by this call rather than a follow-up read.", maxTransferBytes),
	}, sess.fileDownload)
}

func (s *aishwndSession) fileUpload(ctx context.Context, req *mcp.CallToolRequest, args transferArgs) (*mcp.CallToolResult, transferResult, error) {
	if args.LocalPath == "" || args.RemotePath == "" {
		return nil, transferResult{}, errors.New("local_path and remote_path must not be empty")
	}
	data, err := os.ReadFile(args.LocalPath)
	if err != nil {
		// Name the side that failed. A bare "no such file or directory" is
		// exactly as plausible for a mistyped path as for a path from the
		// wrong machine, and local/remote confusion is likeliest here.
		return nil, transferResult{}, fmt.Errorf("reading local_path on the Linux side: %w", err)
	}
	if len(data) > maxTransferBytes {
		return nil, transferResult{}, fmt.Errorf("local file is %d bytes, exceeding the file_upload limit of %d; chunk it with file_read/file_write instead", len(data), maxTransferBytes)
	}
	// No if_match: file_upload is a fresh write, not a compare-and-swap,
	// matching aish's own writeFileAtomic(ctx, rt, path, data, "", "") call
	// for this same tool.
	n, err := s.writeRemoteFile(args.RemotePath, data, "", "", false)
	if err != nil {
		return nil, transferResult{}, fmt.Errorf("writing remote_path on the Windows host: %w", err)
	}
	return nil, transferResult{Bytes: int64(n), Via: "aishwin", Host: s.displayHost(), Sha256: sha256Hex(data)}, nil
}

func (s *aishwndSession) fileDownload(ctx context.Context, req *mcp.CallToolRequest, args transferArgs) (*mcp.CallToolResult, transferResult, error) {
	if args.LocalPath == "" || args.RemotePath == "" {
		return nil, transferResult{}, errors.New("local_path and remote_path must not be empty")
	}
	data, eof, err := s.readRemoteFile(args.RemotePath, 0, maxTransferBytes)
	if err != nil {
		return nil, transferResult{}, fmt.Errorf("reading remote_path on the Windows host: %w", err)
	}
	if !eof {
		return nil, transferResult{}, fmt.Errorf("remote file exceeds the file_download limit of %d bytes; chunk it with file_read/file_write instead", maxTransferBytes)
	}
	// Not atomic (plain os.WriteFile), matching aish's own channel-route
	// file_download — only its SFTP route (which aishwin has no equivalent
	// of) writes via temp+rename.
	if err := os.WriteFile(args.LocalPath, data, 0o644); err != nil {
		return nil, transferResult{}, fmt.Errorf("writing local_path on the Linux side: %w", err)
	}
	return nil, transferResult{Bytes: int64(len(data)), Via: "aishwin", Host: s.displayHost(), Sha256: sha256Hex(data)}, nil
}

// sha256Hex is the same "sha256:" version token file_read already returns, so
// a transfer can be verified against a later read without another round trip.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
