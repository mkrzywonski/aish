package aicmdd

import (
	"context"
	"encoding/json"
	"testing"

	"ai-ssh/internal/aishwinwire"
)

func TestFileGrepRoundTrip(t *testing.T) {
	sess := newTestWirePair(t, "test-fg", func(t *testing.T, peer *aishwinwire.Conn, f aishwinwire.Frame) {
		var req aishwinwire.GrepData
		if err := json.Unmarshal(f.Data, &req); err != nil {
			t.Error(err)
			return
		}
		if req.Pattern != "hello" {
			t.Errorf("Pattern = %q, want %q", req.Pattern, "hello")
		}
		data, _ := json.Marshal(aishwinwire.GrepResultData{
			Matches: []aishwinwire.GrepMatchData{{Path: `C:\a.go`, Line: 2, Text: "func hello() {}"}},
		})
		_ = peer.Send(aishwinwire.Frame{Type: "file_grep_result", ID: f.ID, Data: data})
	})

	_, res, err := sess.fileGrep(context.Background(), nil, fileGrepArgs{Path: `C:\`, Pattern: "hello"})
	if err != nil {
		t.Fatalf("fileGrep: %v", err)
	}
	if len(res.Matches) != 1 || res.Matches[0].Line != 2 {
		t.Errorf("Matches = %+v", res.Matches)
	}
	if res.Via != "aishwin" {
		t.Errorf("Via = %q, want %q", res.Via, "aishwin")
	}
}

func TestFileSearchRoundTrip(t *testing.T) {
	sess := newTestWirePair(t, "test-fs2", func(t *testing.T, peer *aishwinwire.Conn, f aishwinwire.Frame) {
		var req aishwinwire.SearchData
		if err := json.Unmarshal(f.Data, &req); err != nil {
			t.Error(err)
			return
		}
		if req.Name != "*.go" {
			t.Errorf("Name = %q, want %q", req.Name, "*.go")
		}
		data, _ := json.Marshal(aishwinwire.SearchResultData{Paths: []string{`C:\a.go`, `C:\sub\b.go`}})
		_ = peer.Send(aishwinwire.Frame{Type: "file_search_result", ID: f.ID, Data: data})
	})

	_, res, err := sess.fileSearch(context.Background(), nil, fileSearchArgs{Path: `C:\`, Name: "*.go"})
	if err != nil {
		t.Fatalf("fileSearch: %v", err)
	}
	if len(res.Paths) != 2 {
		t.Errorf("Paths = %v", res.Paths)
	}
}

func TestFileSearchRejectsInvalidType(t *testing.T) {
	sess := &aicmdSession{id: "test-fs3", name: "win-test"}
	_, _, err := sess.fileSearch(context.Background(), nil, fileSearchArgs{Path: `C:\`, Type: "bogus"})
	if err == nil {
		t.Fatal("expected an error for an invalid type")
	}
}
