package mcpserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"ai-ssh/internal/term"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func readPtr[T any](v T) *T { return &v }

func TestFileReadPagination(t *testing.T) {
	c := localOOBCore(t)
	path := filepath.Join(t.TempDir(), "source.txt")
	var data strings.Builder
	for i := 1; i <= 450; i++ {
		fmt.Fprintf(&data, "line %d\r\n", i)
	}
	mustWrite(t, path, data.String())
	_, page, err := c.fileRead(context.Background(), nil, fileReadArgs{Path: path, StartLine: readPtr(int64(241)), Limit: readPtr(int64(160)), LineNumbers: true})
	if err != nil {
		t.Fatal(err)
	}
	if page.Content != nil || page.NumberedContent == nil || !strings.HasPrefix(*page.NumberedContent, "   241\tline 241\r\n") || !strings.HasSuffix(*page.NumberedContent, "   400\tline 400\r\n") {
		t.Fatalf("bad numbered page: %+v", page)
	}
	if *page.StartLine != 241 || *page.EndLine != 400 || *page.NextLine != 401 || page.Eof || !page.Truncated || page.TruncationReason != "limit" || page.Version != "" {
		t.Fatalf("bad metadata: %+v", page)
	}
	if page.NextOffset != int64(strings.Index(data.String(), "line 401\r")) {
		t.Fatalf("next_offset = %d", page.NextOffset)
	}
	_, first, err := c.fileRead(context.Background(), nil, fileReadArgs{Path: path, Limit: readPtr(int64(2))})
	if err != nil || first.Content == nil || *first.Content != "line 1\r\nline 2\r\n" {
		t.Fatalf("limit alone: %+v %v", first, err)
	}
	_, last, err := c.fileRead(context.Background(), nil, fileReadArgs{Path: path, StartLine: readPtr(int64(449))})
	if err != nil || !last.Eof || last.NextLine != nil || *last.Content != "line 449\r\nline 450\r\n" {
		t.Fatalf("last: %+v %v", last, err)
	}
	_, beyond, err := c.fileRead(context.Background(), nil, fileReadArgs{Path: path, StartLine: readPtr(int64(999))})
	if err != nil || !beyond.Eof || beyond.BytesRead != 0 || beyond.Version != "" || beyond.NextOffset != int64(data.Len()) {
		t.Fatalf("beyond: %+v %v", beyond, err)
	}
}

func TestFileReadInputValidation(t *testing.T) {
	for _, args := range []fileReadArgs{
		{}, {Path: "x", Offset: readPtr(int64(-1))}, {Path: "x", Offset: readPtr(int64(math.MaxInt64))},
		{Path: "x", MaxBytes: readPtr(0)}, {Path: "x", MaxBytes: readPtr(-1)}, {Path: "x", MaxBytes: readPtr(maxFileRead + 1)},
		{Path: "x", StartLine: readPtr(int64(0))}, {Path: "x", Limit: readPtr(int64(-1))},
		{Path: "x", StartLine: readPtr(int64(math.MaxInt64)), Limit: readPtr(int64(1))},
		{Path: "x", Offset: readPtr(int64(0)), Limit: readPtr(int64(1))},
		{Path: "x", Offset: readPtr(int64(1)), LineNumbers: true},
	} {
		if _, err := args.options(); err == nil {
			t.Errorf("accepted %+v", args)
		}
	}
}

func TestFileReadRoundTripPages(t *testing.T) {
	c := localOOBCore(t)
	for name, data := range map[string]string{"empty": "", "utf8": "one\r\néclair\n🙂 three", "binary": "a\xff\x00z", "newlines": "\n\n\n"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "file")
			mustWrite(t, path, data)
			var all []byte
			offset := int64(0)
			for calls := 0; ; calls++ {
				if calls > len(data)+1 {
					t.Fatal("non-progressing pagination")
				}
				_, page, err := c.fileRead(context.Background(), nil, fileReadArgs{Path: path, Offset: &offset, MaxBytes: readPtr(2)})
				if err != nil {
					t.Fatal(err)
				}
				part := []byte(*page.Content)
				if page.Encoding == "base64" {
					part, err = base64.StdEncoding.DecodeString(*page.Content)
					if err != nil {
						t.Fatal(err)
					}
				}
				all = append(all, part...)
				if page.BytesRead != len(part) || page.NextOffset != offset+int64(len(part)) {
					t.Fatalf("bad byte accounting: %+v", page)
				}
				if page.Eof {
					break
				}
				if page.NextOffset <= offset || page.Version != "" {
					t.Fatalf("bad continuation: %+v", page)
				}
				offset = page.NextOffset
			}
			if string(all) != data {
				t.Fatalf("reconstructed %q != %q", all, data)
			}
		})
	}
	for _, data := range []string{"", "\n", "a\nb", "a\r\nβ\nlast", strings.Repeat("x\n", 300)} {
		path := filepath.Join(t.TempDir(), "file")
		mustWrite(t, path, data)
		start := int64(1)
		var all strings.Builder
		for calls := 0; ; calls++ {
			if calls > len(data)+1 {
				t.Fatal("non-progressing line pages")
			}
			_, page, err := c.fileRead(context.Background(), nil, fileReadArgs{Path: path, StartLine: &start, Limit: readPtr(int64(2)), MaxBytes: readPtr(16)})
			if err != nil {
				t.Fatal(err)
			}
			all.WriteString(*page.Content)
			if page.Eof {
				break
			}
			if page.NextLine == nil || *page.NextLine <= start {
				t.Fatalf("bad line continuation: %+v", page)
			}
			start = *page.NextLine
		}
		if all.String() != data {
			t.Fatalf("line reconstruction %q != %q", all.String(), data)
		}
	}
}

func TestFileReadBudgetsAndVersions(t *testing.T) {
	c := localOOBCore(t)
	for _, data := range []string{strings.Repeat("\"\t\r\\\x01\n", 10000), strings.Repeat("\n", 50000), strings.Repeat("🙂é\n", 10000), strings.Repeat("\xff", maxFileRead)} {
		for _, numbered := range []bool{false, true} {
			if numbered && strings.HasPrefix(data, "\xff") {
				continue
			}
			path := filepath.Join(t.TempDir(), "file")
			mustWrite(t, path, data)
			_, page, err := c.fileRead(context.Background(), nil, fileReadArgs{Path: path, MaxBytes: readPtr(maxFileRead), LineNumbers: numbered})
			if err != nil {
				t.Fatal(err)
			}
			if fileReadWireSize(page) > fileReadResultBudget || !page.Truncated || page.BytesRead == 0 || page.TruncationReason != "response_budget" || page.Version != "" {
				t.Fatalf("bad budget page: %+v size=%d", page, fileReadWireSize(page))
			}
			if (page.Content == nil) == (page.NumberedContent == nil) {
				t.Fatal("must return exactly one representation")
			}
		}
	}
	path := filepath.Join(t.TempDir(), "small")
	for _, data := range []string{"", "last line without newline", "x\n"} {
		mustWrite(t, path, data)
		_, page, err := c.fileRead(context.Background(), nil, fileReadArgs{Path: path, StartLine: readPtr(int64(1))})
		if err != nil || !page.Eof || *page.Content != data || page.Version != sha256Version([]byte(data)) {
			t.Fatalf("whole file: %+v %v", page, err)
		}
	}
	mustWrite(t, path, "prefix\n"+strings.Repeat("z", 100))
	_, _, err := c.fileRead(context.Background(), nil, fileReadArgs{Path: path, StartLine: readPtr(int64(2)), MaxBytes: readPtr(8)})
	if err == nil || !strings.Contains(err.Error(), "offset: 7") {
		t.Fatalf("long line: %v", err)
	}
	_, _, err = c.fileRead(context.Background(), nil, fileReadArgs{Path: path + "missing"})
	if err == nil {
		t.Fatal("missing file succeeded")
	}
}

func TestRemoteFileReadMatchesLocal(t *testing.T) {
	backends := []string{"sh"}
	if _, err := exec.LookPath("busybox"); err == nil {
		backends = append(backends, "busybox")
	}
	for _, backend := range backends {
		t.Run(backend, func(t *testing.T) {
			var env []string
			if backend == "busybox" {
				bin := t.TempDir()
				bb, _ := exec.LookPath("busybox")
				for _, name := range []string{"head", "tail", "wc"} {
					if err := os.Symlink(bb, filepath.Join(bin, name)); err != nil {
						t.Fatal(err)
					}
				}
				// Some BusyBox builds omit base64; byte reads already probe it
				// independently, so use the host encoder with BusyBox selection.
				env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
			}
			for _, data := range []string{"", "one\r\ntwo\nlast", "\n\n\n", strings.Repeat("long", 10000) + "\nx\n", "a\xffb\n"} {
				path := filepath.Join(t.TempDir(), "a ' file.txt")
				mustWrite(t, path, data)
				for _, args := range []fileReadArgs{
					{Path: path}, {Path: path, Offset: readPtr(int64(2)), MaxBytes: readPtr(8)},
					{Path: path, StartLine: readPtr(int64(2)), Limit: readPtr(int64(1)), MaxBytes: readPtr(8)},
					{Path: path, StartLine: readPtr(int64(999999)), MaxBytes: readPtr(8)},
				} {
					o, err := args.options()
					if err != nil {
						t.Fatal(err)
					}
					local, err := readLocalPage(context.Background(), path, o)
					if err != nil {
						t.Fatal(err)
					}
					script, marker, err := remoteReadScript(path, o)
					if err != nil {
						t.Fatal(err)
					}
					cmd := exec.Command("/bin/sh", "-c", script)
					if env != nil {
						cmd.Env = env
					}
					out, err := cmd.CombinedOutput()
					if err != nil {
						t.Fatalf("%v: %s", err, out)
					}
					remote, err := parseRemoteRead(out, marker, o)
					if err != nil {
						t.Fatalf("%v: %.300s", err, out)
					}
					lp, le := renderFilePage(local, o, "channel", "test", "")
					rp, re := renderFilePage(remote, o, "channel", "test", "")
					if (le == nil) != (re == nil) || (le == nil && !reflect.DeepEqual(lp, rp)) {
						t.Fatalf("local=%+v (%v) remote=%+v (%v)", lp, le, rp, re)
					}
				}
			}
		})
	}
}

func TestRemoteFileReadRejectsProducerFailures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	mustWrite(t, path, "a\nb\n")
	for _, name := range []string{"head", "tail", "wc"} {
		t.Run(name, func(t *testing.T) {
			bin := t.TempDir()
			if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nexit 7\n"), 0700); err != nil {
				t.Fatal(err)
			}
			o, _ := (fileReadArgs{Path: path, StartLine: readPtr(int64(2))}).options()
			script, marker, err := remoteReadScript(path, o)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("/bin/sh", "-c", script)
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
			out, runErr := cmd.CombinedOutput()
			_, parseErr := parseRemoteRead(out, marker, o)
			if runErr == nil && parseErr == nil {
				t.Fatalf("masked %s failure: %s", name, out)
			}
		})
	}
}

func TestRemoteFileReadCapCanSplitTrailer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boundary")
	o, _ := (fileReadArgs{Path: path, MaxBytes: readPtr(8)}).options()
	for size := 0; size < 64; size++ {
		mustWrite(t, path, strings.Repeat("x", size))
		script, marker, err := remoteReadScript(path, o)
		if err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("/bin/sh", "-c", script).CombinedOutput()
		if err != nil {
			t.Fatalf("size=%d: %v %s", size, err, out)
		}
		sample, err := parseRemoteRead(out, marker, o)
		if err != nil {
			t.Fatalf("size=%d: %v", size, err)
		}
		page, err := renderFilePage(sample, o, "channel", "test", "")
		if err != nil || page.BytesRead != min(size, 8) || page.Eof != (size <= 8) || *page.Content != strings.Repeat("x", min(size, 8)) {
			t.Fatalf("size=%d: %+v %v", size, page, err)
		}
	}
}

func TestFileReadMCPContract(t *testing.T) {
	c := localOOBCore(t)
	c.Term = term.NewTerminal(24, 80)
	s := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	registerRemoteTools(s, c)
	registerTools(s, c)
	registerSearchTools(s, c)
	st, ct := mcp.NewInMemoryTransports()
	ss, err := s.Connect(context.Background(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cl := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	cs, err := cl.Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	path := filepath.Join(t.TempDir(), "file")
	mustWrite(t, path, strings.Repeat("\"\\\t\n", 10000))
	res := callTool(t, cs, "file_read", map[string]any{"path": path, "start_line": 1, "limit": 10000, "max_bytes": maxFileRead, "line_numbers": true})
	if res.IsError {
		t.Fatalf("file_read: %+v", res.Content)
	}
	wire, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if len(wire) > fileReadResultBudget {
		t.Fatalf("wire size %d exceeds budget", len(wire))
	}
	structured := decodeResult[map[string]any](t, res)
	if _, ok := structured["content"]; ok {
		t.Fatal("duplicate raw content")
	}
	if _, ok := structured["numbered_content"]; !ok {
		t.Fatal("missing numbered content")
	}
	for _, content := range res.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			var m map[string]any
			if err := json.Unmarshal([]byte(text.Text), &m); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(m, structured) {
				t.Fatal("compatibility text differs")
			}
		}
	}
	bad := callTool(t, cs, "file_read", map[string]any{"path": path, "offset": 0, "limit": 2})
	if !bad.IsError {
		t.Fatal("mixed units accepted by MCP")
	}
	for _, tc := range []struct {
		tool, field string
		args        map[string]any
	}{
		{"file_grep", "matches", map[string]any{"path": path, "pattern": "absent"}},
		{"file_search", "paths", map[string]any{"path": filepath.Dir(path), "name": "absent"}},
		{"directory_list", "entries", map[string]any{"path": t.TempDir()}},
		{"session_status", "other_sessions", map[string]any{}},
	} {
		r := callTool(t, cs, tc.tool, tc.args)
		if r.IsError {
			t.Fatalf("%s: %+v", tc.tool, r.Content)
		}
		m := decodeResult[map[string]json.RawMessage](t, r)
		if !bytes.Equal(m[tc.field], []byte("[]")) {
			t.Fatalf("%s.%s=%s", tc.tool, tc.field, m[tc.field])
		}
	}
}
