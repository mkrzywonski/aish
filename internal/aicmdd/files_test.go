package aicmdd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"testing"

	"ai-ssh/internal/aishwinwire"
)

// newTestWirePair sets up a session wired to a fake Windows peer, with
// ReadLoop already running (see the comment on TestExecToolRoundTrip for why
// that's required). handle is invoked once per request the fake peer
// receives, and must send exactly one reply frame with a matching ID.
func newTestWirePair(t *testing.T, id string, handle func(t *testing.T, peer *aishwinwire.Conn, f aishwinwire.Frame)) *aicmdSession {
	t.Helper()
	peerIn, ourOut := io.Pipe()
	ourIn, peerOut := io.Pipe()
	wire := aishwinwire.NewConn(ourIn, ourOut)
	peer := aishwinwire.NewConn(peerIn, peerOut)
	go wire.ReadLoop(func(aishwinwire.Frame) {})
	go func() {
		f, err := peer.ReadOne()
		if err != nil {
			return
		}
		handle(t, peer, f)
	}()
	return &aicmdSession{id: id, name: "win-test", wire: wire}
}

// newMultiTestWirePair is newTestWirePair for tool handlers that make more
// than one sequential wire round trip (file_edit/file_patch: a file_read
// followed by a file_write). handle is invoked once per request, in order.
func newMultiTestWirePair(t *testing.T, id string, handle func(t *testing.T, peer *aishwinwire.Conn, f aishwinwire.Frame)) *aicmdSession {
	t.Helper()
	peerIn, ourOut := io.Pipe()
	ourIn, peerOut := io.Pipe()
	wire := aishwinwire.NewConn(ourIn, ourOut)
	peer := aishwinwire.NewConn(peerIn, peerOut)
	go wire.ReadLoop(func(aishwinwire.Frame) {})
	go func() {
		for {
			f, err := peer.ReadOne()
			if err != nil {
				return
			}
			handle(t, peer, f)
		}
	}()
	return &aicmdSession{id: id, name: "win-test", wire: wire}
}

func TestFileReadRoundTrip(t *testing.T) {
	sess := newTestWirePair(t, "test-fr", func(t *testing.T, peer *aishwinwire.Conn, f aishwinwire.Frame) {
		var req aishwinwire.FileReadData
		if err := json.Unmarshal(f.Data, &req); err != nil {
			t.Error(err)
			return
		}
		if req.Path != `C:\test.txt` {
			t.Errorf("Path = %q, want %q", req.Path, `C:\test.txt`)
		}
		data, _ := json.Marshal(aishwinwire.FileReadResultData{
			Content: base64.StdEncoding.EncodeToString([]byte("hello")),
			Eof:     true,
		})
		_ = peer.Send(aishwinwire.Frame{Type: "file_read_result", ID: f.ID, Data: data})
	})

	_, res, err := sess.fileRead(context.Background(), nil, fileReadArgs{Path: `C:\test.txt`})
	if err != nil {
		t.Fatalf("fileRead: %v", err)
	}
	if res.Content != "hello" || res.Encoding != "utf8" {
		t.Errorf("Content/Encoding = %q/%q, want %q/%q", res.Content, res.Encoding, "hello", "utf8")
	}
	if res.VersionKind != "sha256" || res.Version == "" {
		t.Errorf("expected a sha256 version token for a full-file read, got kind=%q version=%q", res.VersionKind, res.Version)
	}
	if res.Via != "aishwin" || res.Host != "win-test" {
		t.Errorf("Via/Host = %q/%q, want %q/%q", res.Via, res.Host, "aishwin", "win-test")
	}
}

func TestFileWriteRoundTrip(t *testing.T) {
	sess := newTestWirePair(t, "test-fw", func(t *testing.T, peer *aishwinwire.Conn, f aishwinwire.Frame) {
		var req aishwinwire.FileWriteData
		if err := json.Unmarshal(f.Data, &req); err != nil {
			t.Error(err)
			return
		}
		decoded, _ := base64.StdEncoding.DecodeString(req.Content)
		if string(decoded) != "new content" {
			t.Errorf("decoded content = %q, want %q", decoded, "new content")
		}
		data, _ := json.Marshal(aishwinwire.FileWriteResultData{BytesWritten: len(decoded)})
		_ = peer.Send(aishwinwire.Frame{Type: "file_write_result", ID: f.ID, Data: data})
	})

	_, res, err := sess.fileWrite(context.Background(), nil, fileWriteArgs{Path: `C:\test.txt`, Content: "new content"})
	if err != nil {
		t.Fatalf("fileWrite: %v", err)
	}
	if res.BytesWritten != len("new content") {
		t.Errorf("BytesWritten = %d, want %d", res.BytesWritten, len("new content"))
	}
}

func TestFileStatRoundTrip(t *testing.T) {
	sess := newTestWirePair(t, "test-fs", func(t *testing.T, peer *aishwinwire.Conn, f aishwinwire.Frame) {
		data, _ := json.Marshal(aishwinwire.FileStatResultData{Type: "file", Size: 42, Mode: "0644", ModifiedUnix: 1000})
		_ = peer.Send(aishwinwire.Frame{Type: "file_stat_result", ID: f.ID, Data: data})
	})

	_, res, err := sess.fileStat(context.Background(), nil, fileStatArgs{Path: `C:\test.txt`})
	if err != nil {
		t.Fatalf("fileStat: %v", err)
	}
	if res.Type != "file" || res.Size != 42 {
		t.Errorf("Type/Size = %q/%d, want %q/%d", res.Type, res.Size, "file", 42)
	}
	wantVersion := "mtime-size:1000:42"
	if res.Version != wantVersion || res.VersionKind != "mtime-size" {
		t.Errorf("Version/VersionKind = %q/%q, want %q/%q", res.Version, res.VersionKind, wantVersion, "mtime-size")
	}
}

func TestDirectoryListRoundTrip(t *testing.T) {
	sess := newTestWirePair(t, "test-dl", func(t *testing.T, peer *aishwinwire.Conn, f aishwinwire.Frame) {
		data, _ := json.Marshal(aishwinwire.DirectoryListResultData{
			Entries: []aishwinwire.DirEntryData{
				{Name: "a.txt", Type: "file", Size: 1, ModifiedUnix: 1},
				{Name: "sub", Type: "directory", ModifiedUnix: 2},
			},
		})
		_ = peer.Send(aishwinwire.Frame{Type: "directory_list_result", ID: f.ID, Data: data})
	})

	_, res, err := sess.directoryList(context.Background(), nil, directoryListArgs{Path: `C:\`})
	if err != nil {
		t.Fatalf("directoryList: %v", err)
	}
	if len(res.Entries) != 2 {
		t.Fatalf("len(Entries) = %d, want 2", len(res.Entries))
	}
	if res.Entries[0].Name != "a.txt" || res.Entries[1].Type != "directory" {
		t.Errorf("unexpected entries: %#v", res.Entries)
	}
}
