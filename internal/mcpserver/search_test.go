package mcpserver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ai-ssh/internal/sshmux"
)

func TestParseGrep(t *testing.T) {
	// "path\0line:text" records, newline separated. Path contains a colon to
	// prove the NUL split (not a colon split) picks the path correctly.
	out := "/etc/a:b.conf\x0012:listen 443\n/home/x/y.go\x007:func main() {\n"
	matches, truncated, err := parseGrep([]byte(out), 10)
	if err != nil || truncated {
		t.Fatalf("err=%v truncated=%v", err, truncated)
	}
	if len(matches) != 2 {
		t.Fatalf("got %d matches: %+v", len(matches), matches)
	}
	if matches[0].Path != "/etc/a:b.conf" || matches[0].Line != 12 || matches[0].Text != "listen 443" {
		t.Fatalf("match0 = %+v", matches[0])
	}
	if matches[1].Path != "/home/x/y.go" || matches[1].Line != 7 {
		t.Fatalf("match1 = %+v", matches[1])
	}
}

func TestParseGrepTruncates(t *testing.T) {
	out := "a\x001:x\nb\x002:y\nc\x003:z\n"
	matches, truncated, _ := parseGrep([]byte(out), 2)
	if len(matches) != 2 || !truncated {
		t.Fatalf("got %d matches truncated=%v", len(matches), truncated)
	}
}

func TestGrepLocal(t *testing.T) {
	c := localOOBCore(t)
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a.go"), "package main\nfunc hello() {}\n")
	mustWrite(t, filepath.Join(dir, "b.txt"), "hello there\nHELLO again\n")
	mustWrite(t, filepath.Join(dir, "bin.dat"), "hello\x00binary\n")

	// Plain search across all files.
	_, res, err := c.fileGrep(context.Background(), nil, fileGrepArgs{Path: dir, Pattern: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Via != "local" {
		t.Fatalf("via = %s", res.Via)
	}
	// a.go: none (it's "hello()" -> matches "hello"? yes "func hello()"). b.txt: "hello there".
	// bin.dat is binary and skipped. HELLO (uppercase) not matched (case-sensitive).
	got := map[string]bool{}
	for _, m := range res.Matches {
		got[filepath.Base(m.Path)] = true
	}
	if !got["a.go"] || !got["b.txt"] {
		t.Fatalf("expected matches in a.go and b.txt, got %+v", res.Matches)
	}
	if got["bin.dat"] {
		t.Fatalf("binary file should have been skipped: %+v", res.Matches)
	}

	// include filter: only *.txt
	_, res2, err := c.fileGrep(context.Background(), nil, fileGrepArgs{Path: dir, Pattern: "hello", Include: "*.txt"})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range res2.Matches {
		if filepath.Base(m.Path) != "b.txt" {
			t.Fatalf("include filter leaked %s", m.Path)
		}
	}

	// ignore_case picks up HELLO too.
	_, res3, err := c.fileGrep(context.Background(), nil, fileGrepArgs{Path: dir, Pattern: "hello", Include: "*.txt", IgnoreCase: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res3.Matches) != 2 {
		t.Fatalf("ignore_case expected 2 matches, got %+v", res3.Matches)
	}
}

func TestSearchLocal(t *testing.T) {
	c := localOOBCore(t)
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "main.go"), "x")
	mustWrite(t, filepath.Join(dir, "util.go"), "x")
	mustWrite(t, filepath.Join(dir, "readme.md"), "x")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, "sub", "deep.go"), "x")

	_, res, err := c.fileSearch(context.Background(), nil, fileSearchArgs{Path: dir, Name: "*.go"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Via != "local" {
		t.Fatalf("via = %s", res.Via)
	}
	names := map[string]bool{}
	for _, p := range res.Paths {
		names[filepath.Base(p)] = true
	}
	if !names["main.go"] || !names["util.go"] || !names["deep.go"] {
		t.Fatalf("expected the three .go files, got %v", res.Paths)
	}
	if names["readme.md"] {
		t.Fatalf("readme.md should not match *.go")
	}

	// type filter: directories only
	_, dres, err := c.fileSearch(context.Background(), nil, fileSearchArgs{Path: dir, Type: "directory"})
	if err != nil {
		t.Fatal(err)
	}
	if len(dres.Paths) != 1 || filepath.Base(dres.Paths[0]) != "sub" {
		t.Fatalf("directory search = %v", dres.Paths)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParseGrepColon(t *testing.T) {
	// Plain grep fallback: path:line:text.
	out := "/etc/nginx.conf:12:listen 443\n/etc/nginx.conf:20:  listen 80;\n"
	matches, truncated := parseGrepColon([]byte(out), 10)
	if truncated || len(matches) != 2 {
		t.Fatalf("got %d matches truncated=%v", len(matches), truncated)
	}
	if matches[0].Path != "/etc/nginx.conf" || matches[0].Line != 12 || matches[0].Text != "listen 443" {
		t.Fatalf("match0 = %+v", matches[0])
	}
	if matches[1].Line != 20 || matches[1].Text != "  listen 80;" {
		t.Fatalf("match1 = %+v", matches[1])
	}
}

func TestGrepCommandBackends(t *testing.T) {
	args := fileGrepArgs{Path: "/etc", Pattern: "x", Include: "*.conf", IgnoreCase: true}
	// ripgrep preferred, NUL-framed.
	cmd, null := grepCommand(sshmux.Capabilities{HasRg: true, HasGrep: true, GrepNull: true}, args)
	if !null || !strings.Contains(cmd, "rg ") || !strings.Contains(cmd, "-g '*.conf'") {
		t.Fatalf("rg backend: %q null=%v", cmd, null)
	}
	// grep --null when no rg.
	cmd, null = grepCommand(sshmux.Capabilities{HasGrep: true, GrepNull: true}, args)
	if !null || !strings.Contains(cmd, "grep -rnI --null") {
		t.Fatalf("grep --null backend: %q null=%v", cmd, null)
	}
	// plain grep (BusyBox): colon-framed.
	cmd, null = grepCommand(sshmux.Capabilities{HasGrep: true}, args)
	if null || !strings.Contains(cmd, "grep -rnI") || strings.Contains(cmd, "--null") {
		t.Fatalf("plain grep backend: %q null=%v", cmd, null)
	}
}
