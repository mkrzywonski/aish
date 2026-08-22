package aishwnd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"ai-ssh/internal/aishwinwire"
)

func TestFileUploadRoundTrip(t *testing.T) {
	localPath := filepath.Join(t.TempDir(), "up.txt")
	if err := os.WriteFile(localPath, []byte("upload me"), 0o644); err != nil {
		t.Fatal(err)
	}

	var sawIfMatch, sawAppend = "unset", false
	sess := newMultiTestWirePair(t, "test-up", func(t *testing.T, peer *aishwinwire.Conn, f aishwinwire.Frame) {
		if f.Type != "file_write" {
			t.Errorf("unexpected frame type %q", f.Type)
			return
		}
		var req aishwinwire.FileWriteData
		if err := json.Unmarshal(f.Data, &req); err != nil {
			t.Error(err)
			return
		}
		sawIfMatch = req.IfMatch
		sawAppend = req.Append
		decoded, _ := base64.StdEncoding.DecodeString(req.Content)
		if string(decoded) != "upload me" {
			t.Errorf("uploaded content = %q, want %q", decoded, "upload me")
		}
		data, _ := json.Marshal(aishwinwire.FileWriteResultData{BytesWritten: len(decoded)})
		_ = peer.Send(aishwinwire.Frame{Type: "file_write_result", ID: f.ID, Data: data})
	})

	_, res, err := sess.fileUpload(context.Background(), nil, transferArgs{LocalPath: localPath, RemotePath: `C:\up.txt`})
	if err != nil {
		t.Fatalf("fileUpload: %v", err)
	}
	if res.Bytes != int64(len("upload me")) {
		t.Errorf("Bytes = %d, want %d", res.Bytes, len("upload me"))
	}
	if sawIfMatch != "" {
		t.Errorf("if_match = %q, want empty (upload is a fresh write, not a CAS)", sawIfMatch)
	}
	if sawAppend {
		t.Error("append = true, want false")
	}
}

func TestFileDownloadRoundTrip(t *testing.T) {
	localPath := filepath.Join(t.TempDir(), "down.txt")

	sess := newMultiTestWirePair(t, "test-down", func(t *testing.T, peer *aishwinwire.Conn, f aishwinwire.Frame) {
		if f.Type != "file_read" {
			t.Errorf("unexpected frame type %q", f.Type)
			return
		}
		var req aishwinwire.FileReadData
		if err := json.Unmarshal(f.Data, &req); err != nil {
			t.Error(err)
			return
		}
		if req.MaxBytes != maxTransferBytes {
			t.Errorf("MaxBytes = %d, want %d (maxTransferBytes)", req.MaxBytes, maxTransferBytes)
		}
		data, _ := json.Marshal(aishwinwire.FileReadResultData{
			Content: base64.StdEncoding.EncodeToString([]byte("downloaded content")),
			Eof:     true,
		})
		_ = peer.Send(aishwinwire.Frame{Type: "file_read_result", ID: f.ID, Data: data})
	})

	_, res, err := sess.fileDownload(context.Background(), nil, transferArgs{LocalPath: localPath, RemotePath: `C:\down.txt`})
	if err != nil {
		t.Fatalf("fileDownload: %v", err)
	}
	if res.Bytes != int64(len("downloaded content")) {
		t.Errorf("Bytes = %d, want %d", res.Bytes, len("downloaded content"))
	}
	got, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatalf("reading downloaded file: %v", err)
	}
	if string(got) != "downloaded content" {
		t.Errorf("local file content = %q, want %q", got, "downloaded content")
	}
}

func TestFileDownloadRejectsOversizedFile(t *testing.T) {
	sess := newMultiTestWirePair(t, "test-down2", func(t *testing.T, peer *aishwinwire.Conn, f aishwinwire.Frame) {
		data, _ := json.Marshal(aishwinwire.FileReadResultData{Content: base64.StdEncoding.EncodeToString([]byte("partial")), Eof: false})
		_ = peer.Send(aishwinwire.Frame{Type: "file_read_result", ID: f.ID, Data: data})
	})

	_, _, err := sess.fileDownload(context.Background(), nil, transferArgs{LocalPath: "/tmp/whatever", RemotePath: `C:\huge.bin`})
	if err == nil {
		t.Fatal("expected an error when the remote file exceeds maxTransferBytes (eof=false)")
	}
}
