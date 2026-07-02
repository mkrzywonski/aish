package main

import (
	"fmt"
	"os"
)

// Stubs for later milestones so the dispatch table is complete from day one.

func sshShimMain(args []string) int {
	fmt.Fprintln(os.Stderr, "aish ssh shim: not implemented yet (M4)")
	return 1
}

func proxyMain(args []string) int {
	fmt.Fprintln(os.Stderr, "aish mcp-proxy: not implemented yet (M2)")
	return 1
}

func clientMain(args []string) int {
	fmt.Fprintln(os.Stderr, "aish client: not implemented yet (M2)")
	return 1
}
