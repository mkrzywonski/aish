package aishwnd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"ai-ssh/internal/aishwinwire"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func pagePtr[T any](v T) *T { return &v }

// Exercise the MCP daemon's real wire request and response handling. The
// native executor's filesystem selection is tested in cmd/aishwin.
func readPagePeer(t *testing.T, content string) *aishwndSession {
	return newMultiTestWirePair(t, "read-pages", func(t *testing.T, peer *aishwinwire.Conn, f aishwinwire.Frame) {
		var req aishwinwire.FileReadData
		if err := json.Unmarshal(f.Data, &req); err != nil {
			t.Error(err)
			return
		}
		offset := req.Offset
		if req.StartLine > 0 {
			for line := int64(1); line < req.StartLine && offset < int64(len(content)); line++ {
				i := strings.IndexByte(content[offset:], '\n')
				if i < 0 {
					offset = int64(len(content))
					break
				}
				offset += int64(i + 1)
			}
		}
		start := min(offset, int64(len(content)))
		end := min(start+int64(req.MaxBytes), int64(len(content)))
		data, _ := json.Marshal(aishwinwire.FileReadResultData{Content: base64.StdEncoding.EncodeToString([]byte(content[start:end])), Eof: end == int64(len(content)), StartLine: req.StartLine, SourceOffset: offset})
		if err := peer.Send(aishwinwire.Frame{Type: "file_read_result", ID: f.ID, Data: data}); err != nil {
			t.Error(err)
		}
	})
}

func TestFileReadLinePagination(t *testing.T) {
	var content strings.Builder
	for i := 1; i <= 450; i++ {
		fmt.Fprintf(&content, "line %d\r\n", i)
	}
	sess := readPagePeer(t, content.String())
	_, out, err := sess.fileRead(context.Background(), nil, fileReadArgs{Path: `C:\test.txt`, StartLine: pagePtr(int64(241)), Limit: pagePtr(160), LineNumbers: true})
	if err != nil {
		t.Fatal(err)
	}
	if out.Content != nil || out.NumberedContent == nil || !strings.HasPrefix(*out.NumberedContent, "   241\tline 241\r\n") || !strings.HasSuffix(*out.NumberedContent, "   400\tline 400\r\n") {
		t.Fatalf("bad page: %+v", out)
	}
	if out.StartLine != 241 || out.EndLine != 400 || out.NextLine != 401 || out.Eof || !out.Truncated || out.TruncationReason != "limit" || out.Version != "" {
		t.Fatalf("bad metadata: %+v", out)
	}
	if out.NextOffset != int64(strings.Index(content.String(), "line 401\r")) {
		t.Fatalf("next_offset=%d", out.NextOffset)
	}
	_, out, err = sess.fileRead(context.Background(), nil, fileReadArgs{Path: `C:\test.txt`, Limit: pagePtr(2)})
	if err != nil || *out.Content != "line 1\r\nline 2\r\n" {
		t.Fatalf("limit alone: %+v %v", out, err)
	}
}

func TestFileReadValidation(t *testing.T) {
	for _, args := range []fileReadArgs{
		{}, {Path: "x", Offset: pagePtr(int64(-1))}, {Path: "x", Offset: pagePtr(int64(math.MaxInt64))},
		{Path: "x", Offset: pagePtr(int64(0)), Limit: pagePtr(2)}, {Path: "x", Offset: pagePtr(int64(1)), LineNumbers: true},
		{Path: "x", StartLine: pagePtr(int64(0))}, {Path: "x", StartLine: pagePtr(int64(math.MaxInt64))},
		{Path: "x", Limit: pagePtr(0)}, {Path: "x", Limit: pagePtr(10001)},
		{Path: "x", MaxBytes: pagePtr(0)}, {Path: "x", MaxBytes: pagePtr(-1)}, {Path: "x", MaxBytes: pagePtr(maxFileReadBytes + 1)},
	} {
		if _, err := validateFileRead(args); err == nil {
			t.Errorf("accepted %+v", args)
		}
	}
}

func TestFileReadPageReconstruction(t *testing.T) {
	for _, content := range []string{"", "a\r\néclair\n🙂 last", "\n\n\n", "a\xffb\x00c"} {
		sess := readPagePeer(t, content)
		var got []byte
		offset := int64(0)
		for calls := 0; ; calls++ {
			if calls > len(content)+1 {
				t.Fatal("no progress")
			}
			_, out, err := sess.fileRead(context.Background(), nil, fileReadArgs{Path: "x", Offset: &offset, MaxBytes: pagePtr(2)})
			if err != nil {
				t.Fatal(err)
			}
			part := []byte(*out.Content)
			if out.Encoding == "base64" {
				part, err = base64.StdEncoding.DecodeString(*out.Content)
				if err != nil {
					t.Fatal(err)
				}
			}
			got = append(got, part...)
			if out.NextOffset != offset+int64(len(part)) || out.BytesRead != len(part) {
				t.Fatalf("wrong byte count: %+v", out)
			}
			if out.Eof {
				break
			}
			if out.NextOffset <= offset || out.Version != "" {
				t.Fatalf("no progress: %+v", out)
			}
			offset = out.NextOffset
		}
		if string(got) != content {
			t.Fatalf("got %q want %q", got, content)
		}
	}
	for _, content := range []string{"", "a\nb", "a\r\nβ\nlast", strings.Repeat("x\n", 300)} {
		sess := readPagePeer(t, content)
		start := int64(1)
		var got strings.Builder
		for calls := 0; ; calls++ {
			if calls > len(content)+1 {
				t.Fatal("no line progress")
			}
			_, out, err := sess.fileRead(context.Background(), nil, fileReadArgs{Path: "x", StartLine: &start, Limit: pagePtr(2), MaxBytes: pagePtr(16)})
			if err != nil {
				t.Fatal(err)
			}
			got.WriteString(*out.Content)
			if out.Eof {
				break
			}
			if out.NextLine <= start {
				t.Fatalf("no line progress: %+v", out)
			}
			start = out.NextLine
		}
		if got.String() != content {
			t.Fatalf("got %q want %q", got.String(), content)
		}
	}
}

func TestFileReadVersionAndLongLine(t *testing.T) {
	for _, content := range []string{"", "last line", "one\n"} {
		sess := readPagePeer(t, content)
		_, out, err := sess.fileRead(context.Background(), nil, fileReadArgs{Path: "x", StartLine: pagePtr(int64(1))})
		if err != nil || !out.Eof || *out.Content != content || out.Version != aishwinwire.SHA256Version([]byte(content)) {
			t.Fatalf("whole file: %+v %v", out, err)
		}
		_, out, err = sess.fileRead(context.Background(), nil, fileReadArgs{Path: "x", StartLine: pagePtr(int64(100))})
		if err != nil || !out.Eof || out.BytesRead != 0 || out.Version != "" {
			t.Fatalf("beyond EOF: %+v %v", out, err)
		}
	}
	sess := readPagePeer(t, "prefix\n"+strings.Repeat("z", 100))
	_, _, err := sess.fileRead(context.Background(), nil, fileReadArgs{Path: "x", StartLine: pagePtr(int64(2)), MaxBytes: pagePtr(8)})
	if err == nil || !strings.Contains(err.Error(), "offset=7") {
		t.Fatalf("long line: %v", err)
	}
}

func TestFileReadOldPeerAndInvalidReplies(t *testing.T) {
	for name, reply := range map[string]aishwinwire.FileReadResultData{
		"old peer":    {Content: base64.StdEncoding.EncodeToString([]byte("wrong first line")), Eof: true},
		"offset":      {StartLine: 2, SourceOffset: -1, Eof: true},
		"base64":      {StartLine: 2, Content: "@invalid", Eof: true},
		"oversize":    {StartLine: 2, Content: base64.StdEncoding.EncodeToString([]byte("abcde")), Eof: true},
		"no progress": {StartLine: 2, Eof: false},
	} {
		t.Run(name, func(t *testing.T) {
			sess := newTestWirePair(t, name, func(t *testing.T, peer *aishwinwire.Conn, f aishwinwire.Frame) {
				data, _ := json.Marshal(reply)
				_ = peer.Send(aishwinwire.Frame{Type: "file_read_result", ID: f.ID, Data: data})
			})
			_, _, err := sess.fileRead(context.Background(), nil, fileReadArgs{Path: "x", StartLine: pagePtr(int64(2)), MaxBytes: pagePtr(4)})
			if err == nil {
				t.Fatal("invalid peer response accepted")
			}
		})
	}
}

func TestFileReadBoundedMCP(t *testing.T) {
	for _, content := range []string{strings.Repeat("\"\t\r\\\x01\n", 10000), strings.Repeat("\n", 50000), strings.Repeat("🙂é\n", 10000)} {
		sess := readPagePeer(t, content)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil)
		registerFileTools(server, sess)
		server.AddReceivingMiddleware(visibilityMiddleware(), annotateSchemaMiddleware())
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
		out, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "file_read", Arguments: map[string]any{"path": "x", "max_bytes": maxFileReadBytes, "line_numbers": true}})
		if err != nil || out.IsError {
			t.Fatalf("MCP read: %+v %v", out, err)
		}
		wire, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		if len(wire) > 64<<10 {
			t.Fatalf("response=%d bytes", len(wire))
		}
		raw, _ := json.Marshal(out.StructuredContent)
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		if _, ok := fields["content"]; ok {
			t.Fatal("duplicate raw content")
		}
		if _, ok := fields["numbered_content"]; !ok {
			t.Fatal("missing numbered content")
		}
		if !bytes.Equal(fields["visibility"], []byte(`"visible"`)) || !bytes.Equal(fields["truncated"], []byte("true")) {
			t.Fatalf("metadata: %s", raw)
		}
		for _, block := range out.Content {
			if text, ok := block.(*mcp.TextContent); ok {
				var m map[string]json.RawMessage
				if err := json.Unmarshal([]byte(text.Text), &m); err != nil {
					t.Fatal(err)
				}
				if _, ok := m["content"]; ok {
					t.Fatal("text contains duplicate content")
				}
			}
		}
	}
}

func TestDirectoryListEmptyArray(t *testing.T) {
	sess := newTestWirePair(t, "empty-dir", func(t *testing.T, peer *aishwinwire.Conn, f aishwinwire.Frame) {
		_ = peer.Send(aishwinwire.Frame{Type: "directory_list_result", ID: f.ID, Data: json.RawMessage(`{}`)})
	})
	_, res, err := sess.directoryList(context.Background(), nil, directoryListArgs{Path: `C:\`})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(res)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	if string(fields["entries"]) != "[]" {
		t.Fatalf("empty entries: %s", raw)
	}
}
