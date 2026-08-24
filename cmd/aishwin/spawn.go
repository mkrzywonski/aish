//go:build windows

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// spawnFunc starts one attempt at launching the Linux half (cmd/aishwnd),
// returning the running command and its stdin/stdout pipes. The caller
// drives the wire protocol over those pipes and calls cmd.Wait to detect
// when the link drops (process exited, WSL/ssh hiccup, etc).
type spawnFunc func(ctx context.Context) (cmd *exec.Cmd, stdin io.WriteCloser, stdout io.ReadCloser, err error)

// connDescriptor describes what a spawnFunc actually connects to, returned
// alongside it and captured once (StartConnection, connection.go) rather
// than re-derived later from settings -- settings can change after the
// fact, and the very first connection can come from a --ssh/--wsl CLI
// override that never touches settings at all, so re-reading settings
// later could describe a different connection than the one actually live.
// Used to enrich the status bar's connected LED tooltip (gui_statusbar.go)
// with how/what it's connected to, not just that it is.
type connDescriptor struct {
	mode   string // connModeWSL or connModeSSH
	target string // ssh: "[user@]host" or "[user@]host:port"; wsl: distro name, or "" for the default distro
}

// spawnWSL launches the Linux half via `wsl.exe -- aishwnd` (or
// `wsl.exe -d <distro> -- aishwnd` if distro is set) — the default path,
// zero-config for the common case since aishwnd only needs to be on PATH
// inside WSL.
func spawnWSL(distro string) (spawnFunc, connDescriptor) {
	spawn := func(ctx context.Context) (*exec.Cmd, io.WriteCloser, io.ReadCloser, error) {
		args := []string{}
		if distro != "" {
			args = append(args, "-d", distro)
		}
		args = append(args, "--", "aishwnd")
		return startPiped(exec.CommandContext(ctx, "wsl.exe", args...))
	}
	return spawn, connDescriptor{mode: connModeWSL, target: distro}
}

// spawnSSH launches the Linux half via `ssh [user@]hostname aishwnd` —
// covers a non-WSL setup, or a genuinely separate remote Linux box. No pty
// is requested, so ssh transparently forwards stdin/stdout to the remote
// command, exactly like wsl.exe does locally. aishwnd must be installed and
// on PATH on the target; the session it creates lives on that machine,
// discoverable by whatever aish proxy runs there.
//
// ssh's own stdin here is the wire-protocol pipe, not a console, so it
// cannot prompt for a password/passphrase/host-key confirmation the normal
// way. SSH_ASKPASS points back at this same exe (askpass.go); the
// AISHWIN_ASKPASS marker only needs to reach that askpass child, but ssh
// inherits our whole environment into it regardless, so setting it here is
// sufficient. SSH_ASKPASS_REQUIRE=force covers OpenSSH versions that would
// otherwise still prefer a tty prompt when one looks reachable; older
// versions simply ignore the unknown variable and use askpass anyway,
// since our stdin already isn't a tty.
func spawnSSH(target string) (spawnFunc, connDescriptor) {
	return sshSpawn([]string{target, "aishwnd"}), connDescriptor{mode: connModeSSH, target: target}
}

// spawnSSHConfig is spawnSSH built from discrete host/port/user settings
// (Settings > Connection) instead of a single pre-formatted
// "[user@]hostname" string -- the shape the GUI collects, so a user who
// has never touched their ssh_config can still set a non-default port.
func spawnSSHConfig(host string, port int, user string) (spawnFunc, connDescriptor) {
	target := host
	if user != "" {
		target = user + "@" + host
	}
	display := target
	args := []string{}
	if port > 0 {
		args = append(args, "-p", strconv.Itoa(port))
		display = fmt.Sprintf("%s:%d", target, port)
	}
	args = append(args, target, "aishwnd")
	return sshSpawn(args), connDescriptor{mode: connModeSSH, target: display}
}

// sshSpawn is the shared ssh-child setup both spawnSSH and spawnSSHConfig
// build on: the SSH_ASKPASS wiring (askpass.go) is identical regardless of
// how the target/port/user were assembled into args.
func sshSpawn(args []string) spawnFunc {
	return func(ctx context.Context) (*exec.Cmd, io.WriteCloser, io.ReadCloser, error) {
		cmd := exec.CommandContext(ctx, "ssh", args...)
		exe, err := os.Executable()
		if err != nil {
			return nil, nil, nil, err
		}
		cmd.Env = append(os.Environ(),
			"AISHWIN_ASKPASS=1",
			"SSH_ASKPASS="+exe,
			"SSH_ASKPASS_REQUIRE=force",
		)
		return startPiped(cmd)
	}
}

// createNoWindow isn't exposed by the standard syscall package on windows
// (unlike CREATE_NEW_PROCESS_GROUP and friends) -- it's a well-known,
// stable Win32 CreateProcess flag value, so it's declared directly here.
//
// It matters beyond cosmetics: without it, a console
// subsystem child (wsl.exe, ssh.exe) started from aishwin.exe (itself a
// console subsystem binary, per the Makefile's lack of -H=windowsgui)
// simply shares aishwin's own already-open console rather than allocating
// a new one. ssh then finds a real, attached console and tries to read a
// password from it via the Windows console API directly -- bypassing
// SSH_ASKPASS entirely -- and hangs forever, since nothing is reading
// keystrokes into that console on our behalf. Confirmed live: without this
// flag, ssh reached "Next authentication method: password" and stalled
// indefinitely with no SSH_ASKPASS child ever spawned; with it, the child
// has no console at all, so ssh's own isatty-equivalent check correctly
// reports non-interactive and it uses SSH_ASKPASS as spawnSSH intends.
const createNoWindow = 0x08000000

func startPiped(cmd *exec.Cmd) (*exec.Cmd, io.WriteCloser, io.ReadCloser, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	// nil when this process has no console: as a GUI-subsystem binary launched
	// from Explorer there is nothing to inherit, and passing an invalid handle
	// to CreateProcess risks the spawn itself for diagnostics nobody can read.
	cmd.Stderr = consoleStderr()
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, err
	}
	return cmd, stdin, stdout, nil
}
