// aish is a terminal wrapper that lets a human and an AI agent drive one
// shared shell session. It also masquerades as `ssh` (via a PATH shim
// symlink) to inject ControlMaster options into ssh invocations made from
// inside a session.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"ai-ssh/internal/debugcli"
	"ai-ssh/internal/framing"
	"ai-ssh/internal/mcpserver"
	"ai-ssh/internal/paths"
	"ai-ssh/internal/proxy"
	"ai-ssh/internal/session"
	"ai-ssh/internal/shellintegration"
	"ai-ssh/internal/state"
	"ai-ssh/internal/term"
)

const usage = `aish — AI-shareable terminal

Usage:
  aish [run]          start a shared shell session (default)
  aish mcp-proxy [--session <id>]
                      stdio<->socket MCP proxy for AI agents
  aish client [--session <id>] <tool> [json-args]
                      call an MCP tool on a running session (debug)
  aish version
`

var version = "0.1.0-dev"

func main() {
	// Busybox-style dispatch: when invoked through the PATH shim symlink
	// named "ssh", act as the ssh wrapper.
	if filepath.Base(os.Args[0]) == "ssh" {
		os.Exit(sshShimMain(os.Args[1:]))
	}

	args := os.Args[1:]
	sub := "run"
	if len(args) > 0 {
		sub = args[0]
		args = args[1:]
	}

	switch sub {
	case "run":
		os.Exit(runMain(args))
	case "mcp-proxy":
		os.Exit(proxy.Main(args))
	case "client":
		os.Exit(debugcli.Main(version, args))
	case "version":
		fmt.Println("aish", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "aish: unknown command %q\n%s", sub, usage)
		os.Exit(2)
	}
}

func runMain(args []string) int {
	if os.Getenv("AISH_SESSION") != "" {
		fmt.Fprintln(os.Stderr, "aish: already inside an aish session (AISH_SESSION is set)")
		return 1
	}

	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}

	id := session.NewID()
	dir := paths.SessionDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "aish:", err)
		return 1
	}
	defer os.RemoveAll(dir)
	sock := paths.Socket(id)

	argv, shellEnv, err := shellintegration.Setup(shell, dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aish: shell integration disabled: %v\n", err)
		argv, shellEnv = []string{shell}, nil
	}

	extraEnv := append([]string{
		"AISH_SESSION=" + id,
		"AISH_SOCKET=" + sock,
	}, shellEnv...)

	sess := session.New(id, argv, extraEnv)
	trm := term.NewTerminal(24, 80)
	sess.AddTap(trm)
	sess.OnResize(func(rows, cols uint16) {
		trm.Screen.Resize(int(rows), int(cols))
	})

	tracker := state.NewTracker(func() int {
		if sess.Ptmx == nil {
			return -1
		}
		return int(sess.Ptmx.Fd())
	})
	go tracker.Consume(trm.Parser.Subscribe())

	engine := &framing.Engine{Sess: sess, Term: trm, Tracker: tracker}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	core := &mcpserver.Core{Sess: sess, Term: trm, Tracker: tracker, Engine: engine, Version: version}
	go func() {
		if err := mcpserver.Serve(ctx, core, sock); err != nil {
			fmt.Fprintln(os.Stderr, "aish: mcp server:", err)
		}
	}()

	code, err := sess.Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "aish:", err)
	}
	return code
}
