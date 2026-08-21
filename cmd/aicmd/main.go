// aicmd is the Windows half of aicmd: it spawns the Linux half (cmd/aicmdd)
// as a child process — by default via `wsl.exe -- aicmdd`, or via
// `ssh [user@]host aicmdd` with --ssh — and speaks the private wire
// protocol (internal/aicmdwire) over its stdin/stdout, exactly like any
// other stdio MCP server is launched. It owns everything the human
// interacts with for this session: process execution on Windows (later
// stages), file I/O (later stages), and the approval prompts/menu, since
// this is the console the human is actually watching. See the aicmd plan
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
)

var version = "dev"

func main() {
	fs := flag.NewFlagSet("aicmd", flag.ExitOnError)
	sshTarget := fs.String("ssh", "", "spawn the Linux half over ssh instead of wsl.exe, as [user@]hostname")
	distro := fs.String("distro", "", "WSL distro to use with wsl.exe -d (default distro if empty)")
	name := fs.String("name", "", "session name to present to the aish proxy")
	shell := fs.String("shell", "cmd", "persistent shell to drive: cmd or powershell")
	showVersion := fs.Bool("version", false, "print version and exit")
	_ = fs.Parse(os.Args[1:])

	if *showVersion {
		fmt.Fprintln(stdout, "aicmd", version)
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

	fmt.Fprintln(stdout, "aicmd: type 'help' for console commands (rename, access on/off, block on/off, env vars)")

	if err := run(ctx, spawn, *name); err != nil {
		fmt.Fprintln(stderr, "aicmd:", err)
		os.Exit(1)
	}
}
