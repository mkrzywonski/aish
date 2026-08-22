package main

import (
	"os"
	"path/filepath"
	"testing"
)

// These mirror internal/mcpserver/search_test.go's TestGrepLocal/
// TestSearchLocal, calling the ported pure functions (search.go) directly
// rather than through a tool handler — grepLocal/searchLocal are
// unmodified from aish's own local-route implementation (filepath.WalkDir
// + regexp, no shell-out), so the same behavior applies here unchanged.
// The remote-SSH-specific tests in that file (parseGrep, grepCommand
// backend selection) have no equivalent: aishwin never shells out to
// rg/grep/find.

func TestGrepLocal(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a.go"), "package main\nfunc hello() {}\n")
	mustWrite(t, filepath.Join(dir, "b.txt"), "hello there\nHELLO again\n")
	mustWrite(t, filepath.Join(dir, "bin.dat"), "hello\x00binary\n")

	matches, _, err := grepLocal(dir, "hello", "", false, 100)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, m := range matches {
		got[filepath.Base(m.Path)] = true
	}
	if !got["a.go"] || !got["b.txt"] {
		t.Fatalf("expected matches in a.go and b.txt, got %+v", matches)
	}
	if got["bin.dat"] {
		t.Fatalf("binary file should have been skipped: %+v", matches)
	}

	// include filter: only *.txt
	matches2, _, err := grepLocal(dir, "hello", "*.txt", false, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range matches2 {
		if filepath.Base(m.Path) != "b.txt" {
			t.Fatalf("include filter leaked %s", m.Path)
		}
	}

	// ignore_case picks up HELLO too.
	matches3, _, err := grepLocal(dir, "hello", "*.txt", true, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches3) != 2 {
		t.Fatalf("ignore_case expected 2 matches, got %+v", matches3)
	}
}

func TestSearchLocal(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "main.go"), "x")
	mustWrite(t, filepath.Join(dir, "util.go"), "x")
	mustWrite(t, filepath.Join(dir, "readme.md"), "x")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, "sub", "deep.go"), "x")

	paths, _, err := searchLocal(dir, "*.go", "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, p := range paths {
		names[filepath.Base(p)] = true
	}
	if !names["main.go"] || !names["util.go"] || !names["deep.go"] {
		t.Fatalf("expected the three .go files, got %v", paths)
	}
	if names["readme.md"] {
		t.Fatalf("readme.md should not match *.go")
	}

	// type filter: directories only
	dpaths, _, err := searchLocal(dir, "", "directory", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(dpaths) != 1 || filepath.Base(dpaths[0]) != "sub" {
		t.Fatalf("directory search = %v", dpaths)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
