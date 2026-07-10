package mcpserver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"ai-ssh/internal/paths"
	"ai-ssh/internal/session"
	"ai-ssh/internal/sshmux"
	"ai-ssh/internal/state"
)

func localOOBCore(t *testing.T) *Core {
	t.Helper()
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	id := "test-session"
	if err := os.MkdirAll(paths.SessionDir(id), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := paths.GrantOOB(id); err != nil {
		t.Fatal(err)
	}
	return &Core{
		Sess:    &session.Session{ID: id},
		Tracker: state.NewTracker(func() int { return -1 }),
		Mux:     sshmux.New(paths.SessionDir(id)),
		Tasks:   sshmux.NewTable(),
	}
}

func TestLocalFilePrimitives(t *testing.T) {
	c := localOOBCore(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "example.txt")
	if err := os.WriteFile(path, []byte("alpha beta beta\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	_, stat, err := c.fileStat(context.Background(), nil, fileStatArgs{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if stat.Type != "file" || stat.Size != 16 || stat.Mode != "0640" || stat.Via != "local" {
		t.Fatalf("unexpected stat: %+v", stat)
	}

	_, _, err = c.fileEdit(context.Background(), nil, fileEditArgs{Path: path, OldText: "beta", NewText: "gamma"})
	if err == nil {
		t.Fatal("ambiguous edit unexpectedly succeeded")
	}
	_, edit, err := c.fileEdit(context.Background(), nil, fileEditArgs{
		Path: path, OldText: "beta", NewText: "gamma", ReplaceAll: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if edit.Replacements != 2 || edit.Via != "local" {
		t.Fatalf("unexpected edit: %+v", edit)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "alpha gamma gamma\n"; got != want {
		t.Fatalf("edited content = %q, want %q", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode changed to %04o", info.Mode().Perm())
	}

	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, listing, err := c.directoryList(context.Background(), nil, directoryListArgs{Path: dir, MaxEntries: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Entries) != 1 || listing.Entries[0].Name != "example.txt" || !listing.Truncated {
		t.Fatalf("unexpected listing: %+v", listing)
	}
}

func TestExecCwd(t *testing.T) {
	c := localOOBCore(t)
	dir := t.TempDir()
	_, res, err := c.execTool(context.Background(), nil, execArgs{Command: "pwd", Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == nil || *res.ExitCode != 0 || res.Output != dir+"\n" || res.Via != "local" {
		t.Fatalf("unexpected exec result: %+v", res)
	}
}

func TestRemotePrimitiveHelpers(t *testing.T) {
	if got := commandWithCwd("pwd", "/tmp/space here"); got != "cd '/tmp/space here' && pwd" {
		t.Fatalf("commandWithCwd = %q", got)
	}
	for input, want := range map[string]string{
		"regular file":  "file",
		"directory":     "directory",
		"symbolic link": "symlink",
	} {
		if got := remoteFileType(input); got != want {
			t.Errorf("remoteFileType(%q) = %q, want %q", input, got, want)
		}
	}
	if got := normalizePermissions("644"); got != "0644" {
		t.Fatalf("normalizePermissions = %q", got)
	}
}

func TestDivergencePolicy(t *testing.T) {
	cases := []struct {
		confidence string
		kind       opKind
		confirmed  bool
		want       divergenceAction
	}{
		// same host: always allow
		{"same", opRead, false, divAllow},
		{"same", opMutate, false, divAllow},
		// detected mismatch: fail closed for writes, warn for reads
		{"mismatch", opMutate, false, divFail},
		{"mismatch", opMutate, true, divFail}, // a prior confirm never bypasses a detected mismatch
		{"mismatch", opRead, false, divWarn},
		// uncertain: reads proceed silently, writes confirm once
		{"unknown", opRead, false, divAllow},
		{"unknown", opMutate, false, divConfirm},
		{"unknown", opMutate, true, divAllow}, // already confirmed this session
	}
	for _, tc := range cases {
		if got := divergencePolicy(tc.confidence, tc.kind, tc.confirmed); got != tc.want {
			t.Errorf("divergencePolicy(%q, kind=%d, confirmed=%v) = %d, want %d",
				tc.confidence, tc.kind, tc.confirmed, got, tc.want)
		}
	}
}

func TestLocalWriteVersioning(t *testing.T) {
	c := localOOBCore(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "v.txt")
	if err := os.WriteFile(path, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}

	// file_read yields a whole-file sha256 version.
	_, rd, err := c.fileRead(context.Background(), nil, fileReadArgs{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if rd.VersionKind != "sha256" || rd.Version == "" {
		t.Fatalf("expected sha256 version, got %+v", rd)
	}

	// if_match with the current version succeeds.
	_, _, err = c.fileWrite(context.Background(), nil, fileWriteArgs{Path: path, Content: "two", IfMatch: rd.Version})
	if err != nil {
		t.Fatalf("matching if_match write failed: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "two" {
		t.Fatalf("content = %q", got)
	}

	// The old version is now stale: the write must be refused and the file left
	// untouched.
	_, _, err = c.fileWrite(context.Background(), nil, fileWriteArgs{Path: path, Content: "three", IfMatch: rd.Version})
	if err == nil || !errors.Is(err, errStaleWrite) {
		t.Fatalf("stale if_match err = %v, want errStaleWrite", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "two" {
		t.Fatalf("file changed under stale write: %q", got)
	}

	// file_stat yields an mtime-size version that also works as if_match.
	_, st, err := c.fileStat(context.Background(), nil, fileStatArgs{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if st.VersionKind != "mtime-size" || st.Version == "" {
		t.Fatalf("expected mtime-size version, got %+v", st)
	}
	if _, _, err := c.fileWrite(context.Background(), nil, fileWriteArgs{Path: path, Content: "four", IfMatch: st.Version}); err != nil {
		t.Fatalf("mtime-size if_match write failed: %v", err)
	}

	// Mode is preserved across the atomic replace.
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %o, want 644", fi.Mode().Perm())
	}
}

func TestLocalWriteRefusesSymlink(t *testing.T) {
	c := localOOBCore(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "real.txt")
	if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	_, _, err := c.fileWrite(context.Background(), nil, fileWriteArgs{Path: link, Content: "evil"})
	if err == nil || !errors.Is(err, errSymlinkWrite) {
		t.Fatalf("symlink write err = %v, want errSymlinkWrite", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "original" {
		t.Fatalf("symlink target changed: %q", got)
	}
}

func TestLocalEditClosesTOCTOU(t *testing.T) {
	// writeFileAtomic with the version captured at read time must refuse to
	// clobber a file that changed in between (the file_edit TOCTOU guard).
	dir := t.TempDir()
	path := filepath.Join(dir, "race.txt")
	original := []byte("hello world")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	c := localOOBCore(t)
	rt := route{via: "local", host: "local"}
	// A concurrent writer changes the file after we "read" original.
	if err := os.WriteFile(path, []byte("changed underneath"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := c.writeFileAtomic(rt, path, []byte("edited"), "", sha256Version(original))
	if err == nil || !errors.Is(err, errStaleWrite) {
		t.Fatalf("err = %v, want errStaleWrite", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "changed underneath" {
		t.Fatalf("stale edit clobbered concurrent write: %q", got)
	}
}

func TestNumberLines(t *testing.T) {
	got := numberLines([]byte("first\nsecond\nthird\n"))
	want := "     1\tfirst\n     2\tsecond\n     3\tthird\n"
	if got != want {
		t.Fatalf("numberLines =\n%q\nwant\n%q", got, want)
	}
	// No trailing newline: last line still numbered, no spurious empty line.
	if got := numberLines([]byte("a\nb")); got != "     1\ta\n     2\tb\n" {
		t.Fatalf("no-final-newline numberLines = %q", got)
	}
}
