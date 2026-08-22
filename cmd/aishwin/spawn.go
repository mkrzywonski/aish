package main

import (
	"context"
	"io"
	"os"
	"os/exec"
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
func spawnSSH(target string) spawnFunc {
	return func(ctx context.Context) (*exec.Cmd, io.WriteCloser, io.ReadCloser, error) {
		return startPiped(exec.CommandContext(ctx, "ssh", target, "aicmdd"))
	}
}

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
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, err
	}
	return cmd, stdin, stdout, nil
}
