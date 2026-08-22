// aishwin is the Windows half of the aish-driven Windows shell feature: it
// spawns the Linux half (cmd/aicmdd)
// as a child process — by default via `wsl.exe -- aicmdd`, or via
// `ssh [user@]host aicmdd` with --ssh — and speaks the private wire
// protocol (internal/aishwinwire) over its stdin/stdout, exactly like any
// other stdio MCP server is launched. It owns everything the human
// interacts with for this session: process execution on Windows (later
// stages), file I/O (later stages), and the approval prompts/menu, since
// this is the console the human is actually watching. See the aishwin plan
// doc for the full architecture.
//
// This binary must not import internal/mcpserver, internal/session,
// internal/term, or internal/sshmux — those pull in creack/pty, which has
// no Windows support, and this binary is meant to cross-compile with
// GOOS=windows.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
)

var version = "dev"

func main() {
	// Checked before anything else, including flag parsing: when spawnSSH's
	// ssh child needs a password/passphrase/host-key confirmation, it execs
	// $SSH_ASKPASS (this same exe, per spawnSSH) with the prompt text as a
	// plain positional argument -- which flag.Parse would otherwise choke
	// on or misinterpret as a flag. See askpass.go.
	if os.Getenv("AISHWIN_ASKPASS") == "1" {
		runtime.LockOSThread() // dialog creation is thread-affine, same as StartGUI
		os.Exit(runAskPass(strings.Join(os.Args[1:], " ")))
	}

	version = resolveVersion(version)

	fs := flag.NewFlagSet("aishwin", flag.ExitOnError)
	sshTarget := fs.String("ssh", "", "spawn the Linux half over ssh instead of wsl.exe, as [user@]hostname")
	distro := fs.String("distro", "", "WSL distro to use with wsl.exe -d (default distro if empty)")
	name := fs.String("name", "", "session name to present to the aish proxy")
	shell := fs.String("shell", "cmd", "persistent shell to drive: cmd or powershell")
	showVersion := fs.Bool("version", false, "print version and exit")
	guiSmokeTest := fs.Bool("gui-smoke-test", false, "TEMPORARY: show the GUI with fake data and exit, no wire connection")
	_ = fs.Parse(os.Args[1:])

	if *showVersion {
		fmt.Fprintln(stdout, "aishwin", version)
		return
	}

	startScreenshotWatcher()
	startDevControlWatcher()

	if *guiSmokeTest {
		runGUISmokeTest()
		return
	}

	kind := shellCmd
	if *shell == "powershell" {
		kind = shellPowerShell
	}

	execD = newExecDispatcher(kind)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	spawn := spawnWSL(*distro)
	if *sshTarget != "" {
		spawn = spawnSSH(*sshTarget)
	}

	runtime.LockOSThread() // the window-owning thread must pump its own messages

	go func() {
		if err := run(ctx, spawn, *name); err != nil {
			AppendLog(fmt.Sprintf("aishwin: %v", err))
		}
	}()

	refreshStatus()

	title := "aishwin"
	if devBuild {
		title = "aishwin [DEV -- auto-approves, unattended-test channel active]"
	}
	if err := StartGUI(title, buildRealMenu, stop); err != nil {
		fmt.Fprintln(stderr, "aishwin:", err)
		os.Exit(1)
	}
}

// resolveVersion mirrors cmd/aish/main.go's function of the same name (and
// cmd/aicmdd/main.go's copy of it): a build without a linker-injected
// version derives g<revision>[-dirty] from Go's embedded VCS metadata
// instead of reporting a bare "dev" — the difference between, say, two
// native `go build`s of different commits that would otherwise both just
// say "aishwin dev". Duplicated rather than imported since it's unexported in
// package main in each of those.
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
