package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"syscall"
)

// spawnFunc starts one attempt at launching the Linux half (cmd/aicmdd),
// returning the running command and its stdin/stdout pipes. The caller
// drives the wire protocol over those pipes and calls cmd.Wait to detect
// when the link drops (process exited, WSL/ssh hiccup, etc).
type spawnFunc func(ctx context.Context) (cmd *exec.Cmd, stdin io.WriteCloser, stdout io.ReadCloser, err error)

// spawnWSL launches the Linux half via `wsl.exe -- aicmdd` (or
// `wsl.exe -d <distro> -- aicmdd` if distro is set) — the default path,
// zero-config for the common case since aicmdd only needs to be on PATH
// inside WSL.
func spawnWSL(distro string) spawnFunc {
	return func(ctx context.Context) (*exec.Cmd, io.WriteCloser, io.ReadCloser, error) {
		args := []string{}
		if distro != "" {
			args = append(args, "-d", distro)
		}
		args = append(args, "--", "aicmdd")
		return startPiped(exec.CommandContext(ctx, "wsl.exe", args...))
	}
}

// spawnSSH launches the Linux half via `ssh [user@]hostname aicmdd` —
// covers a non-WSL setup, or a genuinely separate remote Linux box. No pty
// is requested, so ssh transparently forwards stdin/stdout to the remote
// command, exactly like wsl.exe does locally. aicmdd must be installed and
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
func spawnSSH(target string) spawnFunc {
	return func(ctx context.Context) (*exec.Cmd, io.WriteCloser, io.ReadCloser, error) {
		cmd := exec.CommandContext(ctx, "ssh", target, "aicmdd")
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
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, err
	}
	return cmd, stdin, stdout, nil
}
