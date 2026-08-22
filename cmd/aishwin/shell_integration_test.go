package main

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestShellAgainstRealWindowsHost drives the real shellSession.Run/
// parseMarker logic against an actual Windows cmd.exe and powershell.exe
// reached over ssh, instead of a synthetic byte stream. There is no Windows
// host in ordinary CI, so this is opt-in: set AISHWIN_TEST_SSH_TARGET to an
// ssh target that resolves to a Windows OpenSSH server (e.g. "localhost" on
// a dev box configured that way) to run it.
func TestShellAgainstRealWindowsHost(t *testing.T) {
	target := os.Getenv("AISHWIN_TEST_SSH_TARGET")
	if target == "" {
		t.Skip("set AISHWIN_TEST_SSH_TARGET (an ssh target reaching a real Windows host) to run this test")
	}

	t.Run("cmd", func(t *testing.T) { testShellRun(t, shellCmd, target) })
	t.Run("powershell", func(t *testing.T) { testShellRun(t, shellPowerShell, target) })
}

func testShellRun(t *testing.T, kind shellKind, target string) {
	t.Helper()

	var remote string
	switch kind {
	case shellPowerShell:
		remote = "powershell.exe -NoLogo -NoProfile"
	default:
		remote = "cmd.exe"
	}
	cmd := exec.Command("ssh", target, remote)
	s, err := newShellSession(kind, cmd)
	if err != nil {
		t.Fatalf("starting %s: %v", kind, err)
	}

	// Successful command.
	out, code, timedOut, err := s.Run("echo hello-from-aishwin-test", 20*time.Second)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if timedOut {
		t.Fatal("unexpected timeout")
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if wantContains := "hello-from-aishwin-test"; !contains(out, wantContains) {
		t.Errorf("output = %q, want it to contain %q", out, wantContains)
	}

	// Failing command reports a nonzero exit code, and cwd/env state
	// persists across calls on the same persistent shell.
	var failCmd, cwdCmd, cwdWant string
	switch kind {
	case shellPowerShell:
		failCmd = "Get-Item C:\\aishwin-test-does-not-exist-xyz"
		cwdCmd = "(Get-Location).Path"
		cwdWant = `C:\Windows`
	default:
		failCmd = "dir C:\\aishwin-test-does-not-exist-xyz"
		cwdCmd = "cd"
		cwdWant = `C:\Windows`
	}

	_, code, timedOut, err = s.Run(failCmd, 20*time.Second)
	if err != nil {
		t.Fatalf("Run (failing command): %v", err)
	}
	if timedOut {
		t.Fatal("unexpected timeout on failing command")
	}
	if code == 0 {
		t.Errorf("exit code = 0 for a command expected to fail")
	}

	// cd persists on the persistent shell across separate Run calls.
	var cdCmd string
	if kind == shellPowerShell {
		cdCmd = "Set-Location C:\\Windows"
	} else {
		cdCmd = "cd /d C:\\Windows"
	}
	if _, _, timedOut, err := s.Run(cdCmd, 20*time.Second); err != nil || timedOut {
		t.Fatalf("Run (cd): timedOut=%v err=%v", timedOut, err)
	}
	out, _, timedOut, err = s.Run(cwdCmd, 20*time.Second)
	if err != nil {
		t.Fatalf("Run (pwd): %v", err)
	}
	if timedOut {
		t.Fatal("unexpected timeout on pwd command")
	}
	if !contains(out, cwdWant) {
		t.Errorf("cwd output = %q, want it to contain %q (cwd should persist across Run calls)", out, cwdWant)
	}

	if s.dead.Load() {
		t.Error("shell unexpectedly marked dead after only successful round trips")
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
