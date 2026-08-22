package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFileOpsAgainstRealWindowsHost exercises the real readFile/
// writeFileAtomic/statFile/listDir functions (files.go) directly against a
// filesystem, not a simulation. Unlike TestShellAgainstRealWindowsHost (a
// pure ssh pipe), these are local os-package calls, so they can't reach a
// remote host from here — instead this test is meant to be cross-compiled
// (`GOOS=windows GOARCH=amd64 go test -c`) and the resulting .test.exe
// copied to and run ON a real Windows host over ssh, to prove the actual
// production code against real NTFS semantics (drive-letter paths,
// os.Rename's atomic-replace behavior, symlink detection) rather than
// assumptions. Set AISHWIN_TEST_FILE_DIR to an existing writable directory to
// run it (skipped otherwise, so plain `go test` on this Linux dev box is
// unaffected).
func TestFileOpsAgainstRealWindowsHost(t *testing.T) {
	dir := os.Getenv("AISHWIN_TEST_FILE_DIR")
	if dir == "" {
		t.Skip("set AISHWIN_TEST_FILE_DIR to a writable directory to run this test (intended to run cross-compiled, on a real Windows host)")
	}

	path := filepath.Join(dir, "aishwin-filetest.txt")
	defer os.Remove(path)

	// Plain write + read round trip.
	if err := writeFileAtomic(path, []byte("hello aishwin"), "", ""); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}
	data, eof, err := readFile(path, 0, 1<<20)
	if err != nil {
		t.Fatalf("readFile: %v", err)
	}
	if !eof {
		t.Error("eof = false, want true for a small file read within max")
	}
	if string(data) != "hello aishwin" {
		t.Errorf("content = %q, want %q", data, "hello aishwin")
	}

	// file_stat: type, size, and a version token that changes across writes.
	kind, _, size, modifiedUnix, err := statFile(path)
	if err != nil {
		t.Fatalf("statFile: %v", err)
	}
	if kind != "file" {
		t.Errorf("type = %q, want %q", kind, "file")
	}
	if size != int64(len("hello aishwin")) {
		t.Errorf("size = %d, want %d", size, len("hello aishwin"))
	}
	if modifiedUnix == 0 {
		t.Error("modified_unix = 0, want a real timestamp")
	}

	// if_match: a stale sha256 token must be rejected...
	if err := writeFileAtomic(path, []byte("nope"), "", "sha256:0000000000000000000000000000000000000000000000000000000000000000"); err == nil {
		t.Error("writeFileAtomic with a stale if_match token succeeded, want errStaleWrite")
	}
	// ...but the correct current token must be accepted, and the write must
	// have actually replaced the content (proving os.Rename's atomic-replace
	// behavior works on this filesystem, not just append-in-place).
	currentVersion, err := fileVersion(path, "sha256:")
	if err != nil {
		t.Fatalf("fileVersion: %v", err)
	}
	if err := writeFileAtomic(path, []byte("replaced content"), "", currentVersion); err != nil {
		t.Fatalf("writeFileAtomic with the correct if_match token: %v", err)
	}
	data, _, err = readFile(path, 0, 1<<20)
	if err != nil {
		t.Fatalf("readFile after replace: %v", err)
	}
	if string(data) != "replaced content" {
		t.Errorf("content after if_match write = %q, want %q", data, "replaced content")
	}

	// Append is not atomic and doesn't check if_match.
	if err := appendWriteHelper(path, []byte(" appended")); err != nil {
		t.Fatalf("appendFile: %v", err)
	}
	data, _, err = readFile(path, 0, 1<<20)
	if err != nil {
		t.Fatalf("readFile after append: %v", err)
	}
	if string(data) != "replaced content appended" {
		t.Errorf("content after append = %q, want %q", data, "replaced content appended")
	}

	// directory_list sees the file we created, sorted, with the right type.
	entries, truncated, err := listDir(dir, 1000)
	if err != nil {
		t.Fatalf("listDir: %v", err)
	}
	if truncated {
		t.Error("truncated = true unexpectedly")
	}
	found := false
	for _, e := range entries {
		if e.Name == filepath.Base(path) {
			found = true
			if e.Type != "file" {
				t.Errorf("listDir entry type = %q, want %q", e.Type, "file")
			}
		}
	}
	if !found {
		t.Errorf("listDir(%q) did not include %q: %#v", dir, filepath.Base(path), entries)
	}
}

func appendWriteHelper(path string, data []byte) error {
	_, err := appendFile(path, data, "")
	return err
}
