package main

import (
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
