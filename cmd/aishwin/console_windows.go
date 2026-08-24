//go:build windows

package main

import "os"

// aishwin is built for the GUI subsystem (-H=windowsgui in the Makefile), so a
// shell that launches it gets its prompt straight back instead of being held
// until the app exits. cmd.exe and PowerShell decide whether to wait from the
// PE header at spawn time, so calling FreeConsole at runtime would not have
// helped — the subsystem is the only lever.
//
// The cost is that a GUI-subsystem process starts with no console at all, so
// `aishwin version` would print into nowhere. AttachConsole(ATTACH_PARENT_PROCESS)
// borrows the launching terminal's console after the fact, which is enough to
// print into it. The shell has already been released by then, so output can
// land after the user's next prompt — acceptable for a version string, and
// better than silence for a startup failure.
//
// Launched from Explorer there is no parent console and the attach simply
// fails, which is the desired outcome: no stray console window appears.

const attachParentProcess = ^uintptr(0) // (DWORD)-1

// kernel32 is already bound in win32.go; reuse it rather than opening the DLL
// a second time.
var (
	procAttachConsole    = kernel32.NewProc("AttachConsole")
	procGetConsoleWindow = kernel32.NewProc("GetConsoleWindow")
)

// haveConsole reports whether this process has a console to write to. Used to
// avoid handing an invalid handle to a child process.
func haveConsole() bool {
	hwnd, _, _ := procGetConsoleWindow.Call()
	return hwnd != 0
}

// attachParentConsole borrows the launching terminal's console, if there is
// one, and points this process's standard output at it.
//
// It must not run on the askpass path: there aishwin is exec'd by ssh with a
// PIPE as stdout, and the password answer is written to it. Rebinding stdout
// would send the answer to a console instead and hang the ssh login. main()
// handles askpass and returns before this is called.
func attachParentConsole() {
	if haveConsole() {
		return
	}
	if r, _, _ := procAttachConsole.Call(attachParentProcess); r == 0 {
		return // no parent console: launched from Explorer, nothing to attach
	}
	out, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0)
	if err != nil {
		return
	}
	os.Stdout, os.Stderr = out, out
	// crlf.go built its writers from the original handles at package init, so
	// they have to be rebuilt against the console we just attached.
	stdout = &crlfWriter{w: out}
	stderr = &crlfWriter{w: out}
}

// consoleStderr returns a stderr for a child process, or nil when this process
// has no console. Passing an invalid handle to CreateProcess is not worth
// risking for diagnostics nobody can see.
func consoleStderr() *os.File {
	if !haveConsole() {
		return nil
	}
	return os.Stderr
}
