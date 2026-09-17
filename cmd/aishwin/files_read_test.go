package main

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A directory used to read back as empty content with eof set, which a caller
// cannot tell apart from a genuinely empty file.
func TestReadFileRefusesDirectory(t *testing.T) {
	dir := t.TempDir()
	_, _, err := readFile(dir, 0, 1024)
	if err == nil {
		t.Fatal("reading a directory returned no error")
	}
	if !strings.Contains(err.Error(), "directory_list") {
		t.Errorf("error should point at the tool that lists directories, got: %v", err)
	}
}

// The empty file is the case the directory read was previously confused with;
// it must still succeed.
func TestReadFileEmptyFileStillSucceeds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.txt")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	data, eof, err := readFile(path, 0, 1024)
	if err != nil {
		t.Fatalf("reading an empty file failed: %v", err)
	}
	if len(data) != 0 || !eof {
		t.Errorf("got %d bytes, eof=%v; want 0 bytes at eof", len(data), eof)
	}
}

func TestReadFileShortReadReachesEOF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "small.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, eof, err := readFile(path, 0, 1024)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(data) != "alpha\nbeta\n" || !eof {
		t.Errorf("got %q eof=%v", data, eof)
	}
}

func TestReadFileLinePageSkipsLongPrefix(t *testing.T) {
	// Every skipped line crosses bufio's 4 KiB buffer boundary, and the
	// prefix exceeds the maximum page size. A byte estimate cannot find 241.
	prefix := strings.Repeat(strings.Repeat("x", 8193)+"\n", 240)
	path := filepath.Join(t.TempDir(), "long-prefix.txt")
	mustWrite(t, path, prefix+"target\r\nfinal")
	data, offset, eof, err := readFileLinePage(path, 241, 256<<10)
	if err != nil || !eof || string(data) != "target\r\nfinal" || offset != int64(len(prefix)) {
		t.Fatalf("line 241 = %q, offset=%d eof=%v err=%v", data, offset, eof, err)
	}
}

func TestReadFileLinePageBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, content, want string
		start, offset       int64
		max                 int
		eof                 bool
	}{
		{name: "empty", start: 1, max: 8, eof: true},
		{name: "empty past eof", start: math.MaxInt64, max: 8, eof: true},
		{name: "crlf no final newline", content: "one\r\ntwo\r\nthree", start: 2, offset: 5, max: 20, want: "two\r\nthree", eof: true},
		{name: "unterminated final line", content: "one\ntwo", start: 2, offset: 4, max: 20, want: "two", eof: true},
		{name: "past unterminated end", content: "one\ntwo", start: 3, offset: 7, max: 20, eof: true},
		{name: "past terminated end", content: "one\ntwo\n", start: 3, offset: 8, max: 20, eof: true},
		{name: "empty first line", content: "\nnext", start: 2, offset: 1, max: 20, want: "next", eof: true},
		{name: "lookahead more data", content: "skip\nabcdef", start: 2, offset: 5, max: 5, want: "abcde", eof: false},
		{name: "exact byte cap", content: "skip\nabcde", start: 2, offset: 5, max: 5, want: "abcde", eof: true},
		{name: "one byte cap", content: "ab", start: 1, max: 1, want: "a", eof: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "input.txt")
			mustWrite(t, path, tc.content)
			data, offset, eof, err := readFileLinePage(path, tc.start, tc.max)
			if err != nil || string(data) != tc.want || offset != tc.offset || eof != tc.eof {
				t.Fatalf("got %q offset=%d eof=%v err=%v; want %q offset=%d eof=%v", data, offset, eof, err, tc.want, tc.offset, tc.eof)
			}
		})
	}
}

func TestReadFileLinePageRejectsInvalidRequests(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input.txt")
	mustWrite(t, path, "text")
	for _, tc := range []struct {
		start int64
		max   int
	}{
		{0, 1}, {-1, 1}, {1, 0}, {1, -1}, {1, (256 << 10) + 1}, {1, math.MaxInt},
	} {
		if _, _, _, err := readFileLinePage(path, tc.start, tc.max); err == nil {
			t.Errorf("accepted start_line=%d max_bytes=%d", tc.start, tc.max)
		}
	}
	if _, _, _, err := readFileLinePage(filepath.Join(t.TempDir(), "missing"), 1, 1); !os.IsNotExist(err) {
		t.Errorf("missing file error = %v", err)
	}
	if _, _, _, err := readFileLinePage(t.TempDir(), 1, 1); err == nil || !strings.Contains(err.Error(), "directory_list") {
		t.Errorf("directory error = %v", err)
	}
}

func TestReadFileValidatesByteBounds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input.txt")
	mustWrite(t, path, "text")
	for _, tc := range []struct {
		offset int64
		max    int
	}{
		{-1, 1}, {0, 0}, {0, -1}, {0, (32 << 20) + 1}, {0, math.MaxInt},
	} {
		if _, _, err := readFile(path, tc.offset, tc.max); err == nil {
			t.Errorf("accepted offset=%d max_bytes=%d", tc.offset, tc.max)
		}
	}
	for _, max := range []int{1 << 20, 32 << 20} {
		// These bounds support file_edit and file_download, respectively.
		data, eof, err := readFile(path, 0, max)
		if err != nil || !eof || string(data) != "text" {
			t.Errorf("max_bytes=%d: data=%q eof=%v err=%v", max, data, eof, err)
		}
	}
	data, eof, err := readFile(path, 1, 1)
	if err != nil || eof || string(data) != "e" {
		t.Fatalf("one-byte offset read = %q eof=%v err=%v", data, eof, err)
	}
}
