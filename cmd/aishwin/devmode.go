//go:build aishwindev

package main

// devmode.go is compiled ONLY into the "aishwindev" build variant
// (`go build -tags aishwindev` / `make aishwin-dev`), never into an
// ordinary build -- devBuild being true is a compile-time fact, not a
// runtime flag, so a normal production aishwin.exe can never accidentally
// skip the human approval gate. See devctl.go for the accompanying
// dev-only remote control channel (menu-by-label invocation, programmatic
// dialog answers) that lets the AI drive the GUI unattended for testing.

const devBuild = true
