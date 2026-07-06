package mcpserver

import (
	"context"
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
