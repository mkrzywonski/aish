package aishwnd

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

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
	if res.Backend != "go" || res.RegexDialect != "go" {
		t.Fatalf("unexpected backend metadata: %+v", res)
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
	sess := &aishwndSession{id: "test-fs3", name: "win-test"}
	_, _, err := sess.fileSearch(context.Background(), nil, fileSearchArgs{Path: `C:\`, Type: "bogus"})
	if err == nil {
		t.Fatal("expected an error for an invalid type")
	}
}

func TestSearchEmptyArraysOverMCP(t *testing.T) {
	for _, tc := range []struct {
		tool, field string
		args        map[string]any
	}{
		{"file_grep", "matches", map[string]any{"path": `C:\`, "pattern": "absent"}},
		{"file_search", "paths", map[string]any{"path": `C:\`, "name": "absent"}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			sess := newTestWirePair(t, "empty-"+tc.tool, func(t *testing.T, peer *aishwinwire.Conn, f aishwinwire.Frame) {
				// The peer's omitempty wire fields omit empty slices. The MCP
				// result must still expose a present array, including with older peers.
				if err := peer.Send(aishwinwire.Frame{Type: tc.tool + "_result", ID: f.ID, Data: json.RawMessage(`{}`)}); err != nil {
					t.Error(err)
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil)
			registerSearchTools(server, sess)
			st, ct := mcp.NewInMemoryTransports()
			ss, err := server.Connect(ctx, st, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer ss.Close()
			client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
			cs, err := client.Connect(ctx, ct, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer cs.Close()
			result, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tc.tool, Arguments: tc.args})
			if err != nil {
				t.Fatal(err)
			}
			if result.IsError {
				t.Fatalf("tool error: %+v", result.Content)
			}
			structured, err := json.Marshal(result.StructuredContent)
			if err != nil {
				t.Fatal(err)
			}
			assertArray := func(data []byte) {
				t.Helper()
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(data, &fields); err != nil {
					t.Fatal(err)
				}
				if string(fields[tc.field]) != "[]" {
					t.Fatalf("%s should be [], got %s", tc.field, data)
				}
			}
			assertArray(structured)
			if len(result.Content) != 1 {
				t.Fatalf("expected one JSON text block, got %d", len(result.Content))
			}
			text, ok := result.Content[0].(*mcp.TextContent)
			if !ok {
				t.Fatalf("unexpected content: %T", result.Content[0])
			}
			assertArray([]byte(text.Text))
		})
	}
}
