package aicmdd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	"ai-ssh/internal/aicmdwire"
)

func TestFileEditRoundTrip(t *testing.T) {
	const original = "func main() {\n\tprintln(\"hi\")\n}\n"
	var sawIfMatch string

	sess := newMultiTestWirePair(t, "test-fe", func(t *testing.T, peer *aicmdwire.Conn, f aicmdwire.Frame) {
		switch f.Type {
		case "file_read":
			var req aicmdwire.FileReadData
			if err := json.Unmarshal(f.Data, &req); err != nil {
				t.Error(err)
				return
			}
			if req.Path != `C:\code.txt` {
				t.Errorf("Path = %q, want %q", req.Path, `C:\code.txt`)
			}
			data, _ := json.Marshal(aicmdwire.FileReadResultData{
				Content: base64.StdEncoding.EncodeToString([]byte(original)),
				Eof:     true,
			})
			_ = peer.Send(aicmdwire.Frame{Type: "file_read_result", ID: f.ID, Data: data})
		case "file_write":
			var req aicmdwire.FileWriteData
			if err := json.Unmarshal(f.Data, &req); err != nil {
				t.Error(err)
				return
			}
			sawIfMatch = req.IfMatch
			decoded, _ := base64.StdEncoding.DecodeString(req.Content)
			if want := "func main() {\n\tprintln(\"hello\")\n}\n"; string(decoded) != want {
				t.Errorf("written content = %q, want %q", decoded, want)
			}
			data, _ := json.Marshal(aicmdwire.FileWriteResultData{BytesWritten: len(decoded)})
			_ = peer.Send(aicmdwire.Frame{Type: "file_write_result", ID: f.ID, Data: data})
		default:
			t.Errorf("unexpected frame type %q", f.Type)
		}
	})

	_, res, err := sess.fileEdit(context.Background(), nil, fileEditArgs{
		Path: `C:\code.txt`, OldText: `println("hi")`, NewText: `println("hello")`,
	})
	if err != nil {
		t.Fatalf("fileEdit: %v", err)
	}
	if res.Replacements != 1 {
		t.Errorf("Replacements = %d, want 1", res.Replacements)
	}
	wantVersion := aicmdwire.SHA256Version([]byte(original))
	if sawIfMatch != wantVersion {
		t.Errorf("file_write's if_match = %q, want %q (the sha256 of what was just read)", sawIfMatch, wantVersion)
	}
}

func TestFileEditRejectsAmbiguousMatch(t *testing.T) {
	sess := newMultiTestWirePair(t, "test-fe2", func(t *testing.T, peer *aicmdwire.Conn, f aicmdwire.Frame) {
		if f.Type != "file_read" {
			t.Errorf("unexpected frame type %q (should have failed before file_write)", f.Type)
			return
		}
		data, _ := json.Marshal(aicmdwire.FileReadResultData{
			Content: base64.StdEncoding.EncodeToString([]byte("a\na\n")),
			Eof:     true,
		})
		_ = peer.Send(aicmdwire.Frame{Type: "file_read_result", ID: f.ID, Data: data})
	})

	_, _, err := sess.fileEdit(context.Background(), nil, fileEditArgs{Path: `C:\f.txt`, OldText: "a", NewText: "b"})
	if err == nil {
		t.Fatal("expected an error for a non-unique old_text without replace_all")
	}
}

func TestFilePatchRoundTrip(t *testing.T) {
	const original = "one\ntwo\nthree\n"
	patch := "@@ -1,3 +1,3 @@\n one\n-two\n+TWO\n three\n"

	sess := newMultiTestWirePair(t, "test-fp", func(t *testing.T, peer *aicmdwire.Conn, f aicmdwire.Frame) {
		switch f.Type {
		case "file_read":
			data, _ := json.Marshal(aicmdwire.FileReadResultData{
				Content: base64.StdEncoding.EncodeToString([]byte(original)),
				Eof:     true,
			})
			_ = peer.Send(aicmdwire.Frame{Type: "file_read_result", ID: f.ID, Data: data})
		case "file_write":
			var req aicmdwire.FileWriteData
			if err := json.Unmarshal(f.Data, &req); err != nil {
				t.Error(err)
				return
			}
			decoded, _ := base64.StdEncoding.DecodeString(req.Content)
			if want := "one\nTWO\nthree\n"; string(decoded) != want {
				t.Errorf("written content = %q, want %q", decoded, want)
			}
			data, _ := json.Marshal(aicmdwire.FileWriteResultData{BytesWritten: len(decoded)})
			_ = peer.Send(aicmdwire.Frame{Type: "file_write_result", ID: f.ID, Data: data})
		default:
			t.Errorf("unexpected frame type %q", f.Type)
		}
	})

	_, res, err := sess.filePatch(context.Background(), nil, filePatchArgs{Path: `C:\f.txt`, Patch: patch})
	if err != nil {
		t.Fatalf("filePatch: %v", err)
	}
	if res.HunksApplied != 1 {
		t.Errorf("HunksApplied = %d, want 1", res.HunksApplied)
	}
}

func TestFilePatchMismatchNeverWrites(t *testing.T) {
	sess := newMultiTestWirePair(t, "test-fp2", func(t *testing.T, peer *aicmdwire.Conn, f aicmdwire.Frame) {
		if f.Type != "file_read" {
			t.Errorf("unexpected frame type %q (should have failed before file_write)", f.Type)
			return
		}
		data, _ := json.Marshal(aicmdwire.FileReadResultData{
			Content: base64.StdEncoding.EncodeToString([]byte("alpha\nbeta\n")),
			Eof:     true,
		})
		_ = peer.Send(aicmdwire.Frame{Type: "file_read_result", ID: f.ID, Data: data})
	})

	patch := "@@ -1,2 +1,2 @@\n-NOT PRESENT\n+x\n beta\n"
	_, _, err := sess.filePatch(context.Background(), nil, filePatchArgs{Path: `C:\f.txt`, Patch: patch})
	if err == nil {
		t.Fatal("expected a patch-mismatch error")
	}
}
