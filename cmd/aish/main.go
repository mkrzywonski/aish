// aish is a terminal wrapper that lets a human and an AI agent drive one
// shared shell session. It also masquerades as `ssh` (via a PATH shim
// symlink) to inject ControlMaster options into ssh invocations made from
// inside a session.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"ai-ssh/internal/session"
)

const usage = `aish — AI-shareable terminal

Usage:
  aish [run]          start a shared shell session (default)
  aish mcp-proxy      stdio<->socket MCP proxy for AI agents
  aish client <tool> [json-args]
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
		os.Exit(proxyMain(args))
	case "client":
		os.Exit(clientMain(args))
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
	extraEnv := []string{"AISH_SESSION=" + id}

	sess := session.New(id, []string{shell}, extraEnv)
	code, err := sess.Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "aish:", err)
	}
	return code
}
