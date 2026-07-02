// aish is a terminal wrapper that lets a human and an AI agent drive one
// shared shell session. It also masquerades as `ssh` (via a PATH shim
// symlink) to inject ControlMaster options into ssh invocations made from
// inside a session.
package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"ai-ssh/internal/debugcli"
	"ai-ssh/internal/framing"
	"ai-ssh/internal/mcpserver"
	"ai-ssh/internal/paths"
	"ai-ssh/internal/proxy"
	"ai-ssh/internal/session"
	"ai-ssh/internal/shellintegration"
	"ai-ssh/internal/sshmux"
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
		os.Exit(sshmux.ShimMain(os.Args[1:]))
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

	sweepStaleSessions()

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

	// Install the ssh PATH shim: a symlink to this binary named "ssh",
	// first on PATH inside the session, so every ssh invocation gains
	// ControlMaster multiplexing.
	shimBin := paths.ShimBin(id)
	if exe, err := os.Executable(); err == nil {
		if err := os.MkdirAll(shimBin, 0o700); err == nil {
			if err := os.Symlink(exe, filepath.Join(shimBin, "ssh")); err == nil {
				extraEnv = append(extraEnv,
					"PATH="+shimBin+":"+os.Getenv("PATH"),
					"AISH_SHIM_BIN="+shimBin)
			}
		}
	}

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

	mux := sshmux.New(dir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	core := &mcpserver.Core{
		Sess: sess, Term: trm, Tracker: tracker, Engine: engine,
		Mux: mux, Tasks: sshmux.NewTable(), Version: version,
	}
	go func() {
		if err := mcpserver.Serve(ctx, core, sock); err != nil {
			fmt.Fprintln(os.Stderr, "aish: mcp server:", err)
		}
	}()

	// The deferred cleanup only runs on a normal return; make external
	// termination (window closed → SIGHUP, kill) clean up too.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sig
		mux.CloseAll()
		os.RemoveAll(dir)
		os.Exit(1)
	}()

	code, err := sess.Run()
	mux.CloseAll()
	if err != nil {
		fmt.Fprintln(os.Stderr, "aish:", err)
	}
	return code
}

// sweepStaleSessions removes runtime dirs of sessions whose MCP socket no
// longer answers (left behind by SIGKILL or a crash).
func sweepStaleSessions() {
	entries, err := os.ReadDir(paths.Base())
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sock := paths.Socket(e.Name())
		conn, err := net.Dial("unix", sock)
		if err == nil {
			conn.Close()
			continue
		}
		os.RemoveAll(paths.SessionDir(e.Name()))
	}
}
