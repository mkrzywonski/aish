package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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
	if !null || !strings.Contains(cmd, "grep -ErnIH --null") {
		t.Fatalf("grep --null backend: %q null=%v", cmd, null)
	}
	// plain grep (BusyBox): colon-framed.
	cmd, null = grepCommand(sshmux.Capabilities{HasGrep: true}, args)
	if null || !strings.Contains(cmd, "grep -ErnIH") || strings.Contains(cmd, "--null") {
		t.Fatalf("plain grep backend: %q null=%v", cmd, null)
	}
}

// Execute the generated shell command and parse its actual output. String-only
// command assertions miss both wrong regex modes and omitted filename framing.
func TestGrepBackendExecution(t *testing.T) {
	backends := []struct {
		name   string
		binary string
		caps   sshmux.Capabilities
	}{
		{"go", "", sshmux.Capabilities{}},
		{"rg", "rg", sshmux.Capabilities{HasRg: true}},
		{"grep_null", "grep", sshmux.Capabilities{HasGrep: true, GrepNull: true}},
		{"grep_colon", "grep", sshmux.Capabilities{HasGrep: true}},
		{"busybox", "busybox", sshmux.Capabilities{HasGrep: true}},
	}
	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			binary := ""
			if backend.binary != "" {
				var err error
				binary, err = exec.LookPath(backend.binary)
				if err != nil {
					t.Skipf("%s is not installed", backend.binary)
				}
			}
			dir := t.TempDir()
			path := filepath.Join(dir, "quote's file.py")
			content := "def alpha():\n    def beta():\nmetrics.prom\nrequests\na|b\nfoo's bar\nDEF UPPER\n-dash\n"
			mustWrite(t, path, content)
			run := func(args fileGrepArgs, max int) ([]grepMatch, bool, error) {
				if backend.name == "go" {
					return grepLocal(args.Path, args.Pattern, args.Include, args.IgnoreCase, max)
				}
				command, null := grepCommand(backend.caps, args)
				if backend.name == "busybox" {
					command = sshmux.Quote(binary) + " grep" + strings.TrimPrefix(command, "grep")
				}
				out, err := exec.Command("sh", "-c", command).CombinedOutput()
				if err != nil {
					if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 1 {
						return nil, false, err
					}
				}
				if null {
					return parseGrep(out, max)
				}
				matches, truncated := parseGrepColon(out, max)
				return matches, truncated, nil
			}
			patterns := []struct {
				name, pattern string
				lines         []int
				ignoreCase    bool
			}{
				{"literal", "def", []int{1, 2}, false},
				{"alternation", `def |\.prom|textfile|requests`, []int{1, 2, 3, 4}, false},
				{"anchors", `^def |^    def `, []int{1, 2}, false},
				{"grouping", `^(def|requests)`, []int{1, 4}, false},
				{"escaped_pipe", `a\|b`, []int{5}, false},
				{"quoted_pattern", "foo's", []int{6}, false},
				{"ignore_case", "^def", []int{1, 7}, true},
				{"leading_dash", "-dash", []int{8}, false},
				{"no_matches", "absent", []int{}, false},
			}
			for _, root := range []struct{ name, path string }{{"file", path}, {"directory", dir}} {
				for _, pattern := range patterns {
					t.Run(root.name+"/"+pattern.name, func(t *testing.T) {
						matches, truncated, err := run(fileGrepArgs{Path: root.path, Pattern: pattern.pattern, IgnoreCase: pattern.ignoreCase}, 100)
						if err != nil || truncated {
							t.Fatalf("truncated=%v err=%v", truncated, err)
						}
						if matches == nil {
							t.Fatal("matches must be a non-nil array")
						}
						lines := make([]int, 0, len(matches))
						for _, match := range matches {
							if match.Path != path {
								t.Fatalf("path = %q, want %q", match.Path, path)
							}
							lines = append(lines, match.Line)
							if match.Text != strings.Split(content, "\n")[match.Line-1] {
								t.Fatalf("incorrect match text: %+v", match)
							}
						}
						if !reflect.DeepEqual(lines, pattern.lines) {
							t.Fatalf("lines=%v want %v", lines, pattern.lines)
						}
					})
				}
			}
			for _, args := range []fileGrepArgs{{Path: path, Pattern: "["}, {Path: filepath.Join(dir, "missing"), Pattern: "def"}} {
				if _, _, err := run(args, 100); err == nil {
					t.Fatalf("expected error for %+v", args)
				}
			}
			matches, truncated, err := run(fileGrepArgs{Path: path, Pattern: "def"}, 1)
			if err != nil || !truncated || len(matches) != 1 {
				t.Fatalf("cap: matches=%v truncated=%v err=%v", matches, truncated, err)
			}
			if backend.name != "busybox" {
				mustWrite(t, filepath.Join(dir, "excluded.txt"), "def excluded\n")
				matches, _, err = run(fileGrepArgs{Path: dir, Pattern: "def", Include: "*.py"}, 100)
				if err != nil || len(matches) != 2 {
					t.Fatalf("include: matches=%v err=%v", matches, err)
				}
			}
			if backend.name == "go" || backend.caps.HasRg || backend.caps.GrepNull {
				colonPath := filepath.Join(dir, "a:b.py")
				mustWrite(t, colonPath, "def colon\n")
				matches, _, err = run(fileGrepArgs{Path: colonPath, Pattern: "def"}, 100)
				if err != nil || len(matches) != 1 || matches[0].Path != colonPath {
					t.Fatalf("colon filename: matches=%v err=%v", matches, err)
				}
			}
		})
	}
}

func TestSearchEmptyResultsAndRootErrors(t *testing.T) {
	c := localOOBCore(t)
	dir := t.TempDir()
	_, grep, err := c.fileGrep(context.Background(), nil, fileGrepArgs{Path: dir, Pattern: "absent"})
	if err != nil {
		t.Fatal(err)
	}
	if grep.Backend != "go" || grep.RegexDialect != "go" {
		t.Fatalf("missing local engine metadata: %+v", grep)
	}
	data, err := json.Marshal(grep)
	if err != nil || !strings.Contains(string(data), `"matches":[]`) {
		t.Fatalf("empty grep: %s, %v", data, err)
	}
	_, search, err := c.fileSearch(context.Background(), nil, fileSearchArgs{Path: dir})
	if err != nil {
		t.Fatal(err)
	}
	data, err = json.Marshal(search)
	if err != nil || !strings.Contains(string(data), `"paths":[]`) {
		t.Fatalf("empty search: %s, %v", data, err)
	}
	missing := filepath.Join(dir, "missing")
	if _, _, err := c.fileGrep(context.Background(), nil, fileGrepArgs{Path: missing, Pattern: "x"}); !os.IsNotExist(err) {
		t.Fatalf("missing grep root: %v", err)
	}
	if _, _, err := c.fileSearch(context.Background(), nil, fileSearchArgs{Path: missing}); !os.IsNotExist(err) {
		t.Fatalf("missing search root: %v", err)
	}
	file := filepath.Join(dir, "file")
	mustWrite(t, file, "x")
	if _, _, err := searchLocal(file, "", "", 10); err == nil {
		t.Fatal("file_search must reject a file root")
	}
}

func TestSearchUnreadableRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read chmod 000 fixtures")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "unreadable")
	mustWrite(t, file, "text")
	if err := os.Chmod(file, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := grepLocal(file, "text", "", false, 10); !os.IsPermission(err) {
		t.Fatalf("unreadable file: %v", err)
	}
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	if _, _, err := grepLocal(dir, "text", "", false, 10); !os.IsPermission(err) {
		t.Fatalf("unreadable grep directory: %v", err)
	}
	if _, _, err := searchLocal(dir, "", "", 10); !os.IsPermission(err) {
		t.Fatalf("unreadable search directory: %v", err)
	}
}

func TestClassifySearchOutput(t *testing.T) {
	for _, tt := range []struct {
		name, data string
		exit       int
		capped     bool
	}{
		{"success", "file\n@AISHRC@0\n", 0, false},
		{"no_match", "@AISHRC@1\n", 1, false},
		{"failure", "invalid regex\n@AISHRC@2\n", 2, false},
		{"capped", "file\npartial", -1, true},
		{"marker_cut", "file\n@AISHRC@", -1, true},
		{"numeric_marker_cut", "file\n@AISHRC@1", -1, true},
		{"invalid_exit", "file\n@AISHRC@oops\n", -1, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, exit, capped := classifySearchOutput([]byte(tt.data))
			if exit != tt.exit || capped != tt.capped {
				t.Fatalf("exit=%d capped=%v", exit, capped)
			}
		})
	}
	if got := string(completeSearchRecords([]byte("/a\x001:x\n/b\x002:cut"), '\n')); got != "/a\x001:x\n" {
		t.Fatalf("partial grep record retained: %q", got)
	}
	if got := string(completeSearchRecords([]byte("/a\x00/partial"), 0)); got != "/a\x00" {
		t.Fatalf("partial path retained: %q", got)
	}
	if got := completeSearchRecords([]byte("partial"), '\n'); len(got) != 0 {
		t.Fatalf("unterminated record retained: %q", got)
	}
}

func TestGrepBackendMetadata(t *testing.T) {
	for _, tt := range []struct {
		caps             sshmux.Capabilities
		backend, dialect string
	}{
		{sshmux.Capabilities{HasRg: true}, "rg", "rust"},
		{sshmux.Capabilities{HasGrep: true, GrepNull: true}, "grep", "posix_ere"},
		{sshmux.Capabilities{HasGrep: true}, "grep", "posix_ere"},
	} {
		backend, dialect := grepBackend(tt.caps)
		if backend != tt.backend || dialect != tt.dialect {
			t.Fatalf("backend=%q dialect=%q", backend, dialect)
		}
	}
}
