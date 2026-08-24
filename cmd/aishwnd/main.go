// aishwnd is the Linux/WSL half of aishwin. aishwin.exe (Windows) spawns it as a
// child process — by default via `wsl.exe -- aishwnd`, or via
// `ssh [user@]host aishwnd` for a non-WSL/remote Linux target — and speaks
// the private wire protocol over its stdin/stdout, exactly like any other
// stdio MCP server (including aish's own `aish mcp-proxy`). It presents an
// ordinary aish-shaped MCP session (visible to the aish proxy's
// list_sessions like any other session) backed by that Windows peer instead
// of a local PTY. See the aishwin plan doc for the full architecture;
// internal/mcpserver, internal/session, internal/sshmux, and internal/term
// are deliberately untouched by this binary.
//
// stdout is the wire protocol channel to the parent process — nothing but
// protocol frames may be written there. Diagnostics go to stderr.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"ai-ssh/internal/aishwnd"
)

var version = "dev"

func main() {
	version = resolveVersion(version)
	aishwnd.Version = version

	// `aishwnd version` mirrors `aish version`. Two binaries in one product
	// answering the same question two different ways is a papercut nobody
	// should have to remember; --version stays for habit and scripts.
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("aishwnd", version)
		return
	}

	fs := flag.NewFlagSet("aishwnd", flag.ExitOnError)
	showVersion := fs.Bool("version", false, "print version and exit")
	_ = fs.Parse(os.Args[1:])

	if *showVersion {
		// Safe on stdout: only reached when not serving the wire protocol.
		fmt.Println("aishwnd", version)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if err := aishwnd.Run(ctx, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "aishwnd:", err)
		os.Exit(1)
	}
}

// resolveVersion mirrors cmd/aish/main.go's function of the same name: a
// build without a linker-injected version derives g<revision>[-dirty] from
// Go's embedded VCS metadata instead of reporting a bare "dev". Duplicated
// rather than imported since it's unexported in package main there.
func resolveVersion(stamped string) string {
	if stamped != "" && stamped != "dev" {
		return stamped
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	return versionFromSettings(info.Settings)
}

func versionFromSettings(settings []debug.BuildSetting) string {
	revision := ""
	dirty := false
	for _, setting := range settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			dirty = setting.Value == "true"
		}
	}
	if revision == "" {
		return "dev"
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	result := "g" + revision
	if dirty {
		result += "-dirty"
	}
	return result
}
