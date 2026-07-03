package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// install/uninstall register the aish MCP server with AI TUIs (Claude Code,
// Codex) by shelling out to each tool's own `mcp add`/`mcp remove` command —
// so aish never has to know their config formats. Both tools take the same
// shape: `<tool> mcp add <name> -- <command> [args...]`.

var supportedTools = []string{"claude", "codex"}

// proxyCommand returns the command an AI TUI should run for the MCP server.
// Prefer bare "aish" when it's on PATH (survives NixOS rebuilds and binary
// moves); otherwise the absolute path to this executable.
func proxyCommand() (string, []string) {
	if _, err := exec.LookPath("aish"); err == nil {
		return "aish", []string{"mcp-proxy"}
	}
	exe, err := os.Executable()
	if err != nil {
		exe = "aish"
	}
	return exe, []string{"mcp-proxy"}
}

// targetsFrom resolves the requested tool arg to a concrete list.
func targetsFrom(args []string) ([]string, error) {
	if len(args) == 0 || args[0] == "all" {
		return supportedTools, nil
	}
	for _, t := range supportedTools {
		if args[0] == t {
			return []string{t}, nil
		}
	}
	return nil, fmt.Errorf("unknown target %q (want: %s, or all)", args[0], strings.Join(supportedTools, ", "))
}

func installMain(args []string) int {
	targets, err := targetsFrom(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "aish:", err)
		return 2
	}
	cmd, cmdArgs := proxyCommand()
	fmt.Printf("Registering aish MCP server as: %s %s\n", cmd, strings.Join(cmdArgs, " "))
	any := false
	for _, t := range targets {
		if _, err := exec.LookPath(t); err != nil {
			fmt.Printf("  %-7s not found on PATH — skipping\n", t)
			continue
		}
		any = true
		// Idempotent: clear any existing entry first so re-running updates it.
		_ = exec.Command(t, "mcp", "remove", "aish").Run()
		add := []string{"mcp", "add", "aish"}
		if t == "claude" {
			add = append(add, "--scope", "user") // global, not per-project
		}
		add = append(add, "--", cmd)
		add = append(add, cmdArgs...)
		if out, err := exec.Command(t, add...).CombinedOutput(); err != nil {
			fmt.Printf("  %-7s failed: %v: %s\n", t, err, strings.TrimSpace(string(out)))
		} else {
			fmt.Printf("  %-7s registered\n", t)
		}
	}
	if !any {
		fmt.Println("No supported AI TUIs found on PATH (claude, codex).")
		return 1
	}
	fmt.Println("Done. Restart the AI TUI (or reconnect it) to pick up the server.")
	return 0
}

func uninstallMain(args []string) int {
	targets, err := targetsFrom(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "aish:", err)
		return 2
	}
	for _, t := range targets {
		if _, err := exec.LookPath(t); err != nil {
			fmt.Printf("  %-7s not found on PATH — skipping\n", t)
			continue
		}
		if out, err := exec.Command(t, "mcp", "remove", "aish").CombinedOutput(); err != nil {
			fmt.Printf("  %-7s not registered (%s)\n", t, strings.TrimSpace(string(out)))
		} else {
			fmt.Printf("  %-7s removed\n", t)
		}
	}
	return 0
}
