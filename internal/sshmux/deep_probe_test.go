package sshmux

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestClassifyDeepProbe(t *testing.T) {
	const marker = "AISH_DIALECT_0123456789abcdef"
	tests := []struct {
		name     string
		stdout   string
		dialect  Dialect
		platform string
	}{
		{
			name: "cmd expansion captured locally",
			stdout: marker + " PCTOS=Windows_NT PCTCOMSPEC=C:\\Windows\\System32\\cmd.exe " +
				"PSOS=$env:OS PSCOMSPEC=$env:ComSpec SH=$SHELL\r\n",
			dialect: DialectCmd, platform: "windows",
		},
		{
			name: "PowerShell expansion captured locally",
			stdout: "profile noise\r\n" + marker + "\r\nPCTOS=%OS%\r\nPCTCOMSPEC=%COMSPEC%\r\n" +
				"PSOS=Windows_NT\r\nPSCOMSPEC=C:\\Windows\\System32\\cmd.exe\r\nSH=\r\n",
			dialect: DialectPowerShell, platform: "windows",
		},
		{
			name:    "PowerShell with unset environment variables",
			stdout:  marker + "\nPCTOS=%OS%\nPCTCOMSPEC=%COMSPEC%\nPSOS=\nPSCOMSPEC=\nSH=\n",
			dialect: DialectPowerShell, platform: "windows",
		},
		{
			name:    "POSIX expansion captured locally",
			stdout:  marker + " PCTOS=%OS% PCTCOMSPEC=%COMSPEC% PSOS=:OS PSCOMSPEC=:ComSpec SH=/bin/bash\n",
			dialect: DialectPosix, platform: "unix",
		},
		{
			name:    "POSIX with unset SHELL",
			stdout:  marker + " PCTOS=%OS% PCTCOMSPEC=%COMSPEC% PSOS=:OS PSCOMSPEC=:ComSpec SH=\n",
			dialect: DialectPosix, platform: "unix",
		},
		{name: "marker missing", stdout: "PCTOS=Windows_NT\n"},
		{name: "incomplete", stdout: marker + " PCTOS=Windows_NT\n"},
		{
			name:   "mixed unrecognized expansion",
			stdout: marker + " PCTOS=%OS% PCTCOMSPEC=cmd.exe PSOS=:OS PSCOMSPEC=:ComSpec SH=/bin/sh\n",
		},
		{
			name: "trailing profile noise cannot override response",
			stdout: marker + " PCTOS=Windows_NT PCTCOMSPEC=C:\\Windows\\System32\\cmd.exe " +
				"PSOS=$env:OS PSCOMSPEC=$env:ComSpec SH=$SHELL\n" +
				"PCTOS=%OS% PCTCOMSPEC=%COMSPEC% PSOS=:OS PSCOMSPEC=:ComSpec SH=/bin/sh\n",
			dialect: DialectCmd, platform: "windows",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dialect, platform, evidence, reason := classifyDeepProbe([]byte(tc.stdout), marker)
			if dialect != tc.dialect || platform != tc.platform {
				t.Errorf("identity = %q/%q, want %q/%q", dialect, platform, tc.dialect, tc.platform)
			}
			if tc.dialect == DialectUnknown && reason == "" {
				t.Error("unknown result should explain why it was not classified")
			}
			if tc.dialect != DialectUnknown && evidence == "" {
				t.Error("identified result should carry evidence")
			}
		})
	}
}

func TestBuildDeepProbeCommandUsesSafeUniqueMarker(t *testing.T) {
	first, marker1, err := buildDeepProbeCommand()
	if err != nil {
		t.Fatal(err)
	}
	second, marker2, err := buildDeepProbeCommand()
	if err != nil {
		t.Fatal(err)
	}
	if marker1 == marker2 {
		t.Fatal("deep probe markers should be unique")
	}
	for _, marker := range []string{marker1, marker2} {
		if !strings.HasPrefix(marker, "AISH_DIALECT_") || strings.ContainsAny(marker, " \t\r\n;&|$%") {
			t.Errorf("marker is not cross-shell safe: %q", marker)
		}
	}
	for _, command := range []string{first, second} {
		for _, field := range []string{"PCTOS=%OS%", "PCTCOMSPEC=%COMSPEC%", "PSOS=$env:OS", "PSCOMSPEC=$env:ComSpec", "SH=$SHELL"} {
			if !strings.Contains(command, field) {
				t.Errorf("command missing %q: %s", field, command)
			}
		}
	}
}

func TestDeepProbeCachesIdentityWithoutChangingShell(t *testing.T) {
	m := New(t.TempDir())
	ci := testConn()
	var calls atomic.Int32
	m.deepRun = func(_ context.Context, _ *ConnInfo, command string) deepCommandResult {
		calls.Add(1)
		marker := strings.Fields(command)[1]
		return deepCommandResult{Stdout: []byte(marker + " PCTOS=Windows_NT PCTCOMSPEC=C:\\Windows\\cmd.exe PSOS=$env:OS PSCOMSPEC=$env:ComSpec SH=$SHELL\n"), Exit: 0}
	}

	first, err := m.DeepProbe(context.Background(), ci, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.DeepProbe(context.Background(), ci, false)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("runner calls = %d, want 1", calls.Load())
	}
	if first.Status != DeepProbeIdentified || first.Dialect != DialectCmd || first.Cached {
		t.Errorf("first result = %+v", first)
	}
	if !second.Cached || second.Dialect != DialectCmd {
		t.Errorf("cached result = %+v", second)
	}
	f, _ := m.Facts(ci)
	if f.Shell.State != AxisUnknown {
		t.Errorf("deep identity changed shell state to %s", f.Shell.State)
	}
	if identity := f.Identity.DialectFact(); identity.Dialect != DialectCmd || identity.Source != IdentitySourceDeepProbe {
		t.Errorf("deep identity was not recorded authoritatively: %+v", identity)
	}
}

func TestDeepProbeForceRetriesOnlyDeepState(t *testing.T) {
	m := New(t.TempDir())
	ci := testConn()
	m.NoteShellUsable(ci, Capabilities{Hostname: "buildbox", HasBase64: true})
	var calls atomic.Int32
	m.deepRun = func(_ context.Context, _ *ConnInfo, command string) deepCommandResult {
		calls.Add(1)
		marker := strings.Fields(command)[1]
		return deepCommandResult{Stdout: []byte(marker + " PCTOS=%OS% PCTCOMSPEC=%COMSPEC% PSOS=:OS PSCOMSPEC=:ComSpec SH=/bin/sh\n"), Exit: 0}
	}

	if _, err := m.DeepProbe(context.Background(), ci, false); err != nil {
		t.Fatal(err)
	}
	forced, err := m.DeepProbe(context.Background(), ci, true)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || forced.Cached {
		t.Errorf("forced result = %+v, runner calls = %d", forced, calls.Load())
	}
	f, _ := m.Facts(ci)
	if !f.ShellUsable() || f.Shell.Caps.Hostname != "buildbox" {
		t.Errorf("deep force reset shell facts: %+v", f.Shell)
	}
}

func TestDeepProbeCachesFailure(t *testing.T) {
	m := New(t.TempDir())
	ci := testConn()
	var calls atomic.Int32
	m.deepRun = func(context.Context, *ConnInfo, string) deepCommandResult {
		calls.Add(1)
		return deepCommandResult{Stderr: []byte("remote command refused\n"), Exit: 255, Err: errors.New("exit status 255")}
	}

	first, err := m.DeepProbe(context.Background(), ci, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.DeepProbe(context.Background(), ci, false)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != DeepProbeFailed || !strings.Contains(first.Reason, "remote command refused") {
		t.Errorf("failed result = %+v", first)
	}
	if calls.Load() != 1 || !second.Cached {
		t.Errorf("failed result was retried: calls=%d second=%+v", calls.Load(), second)
	}
}

func TestDeepProbeCachesUnknownAndTimeout(t *testing.T) {
	tests := []struct {
		name   string
		result deepCommandResult
		status DeepProbeStatus
		reason string
	}{
		{
			name: "unknown response", result: deepCommandResult{Stdout: []byte("login banner only\n"), Exit: 0},
			status: DeepProbeUnknown, reason: "marker",
		},
		{
			name: "timeout", result: deepCommandResult{Exit: -1, TimedOut: true, Err: context.DeadlineExceeded},
			status: DeepProbeFailed, reason: "timed out",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := New(t.TempDir())
			ci := testConn()
			var calls atomic.Int32
			m.deepRun = func(context.Context, *ConnInfo, string) deepCommandResult {
				calls.Add(1)
				return tc.result
			}

			first, err := m.DeepProbe(context.Background(), ci, false)
			if err != nil {
				t.Fatal(err)
			}
			second, err := m.DeepProbe(context.Background(), ci, false)
			if err != nil {
				t.Fatal(err)
			}
			if first.Status != tc.status || !strings.Contains(first.Reason, tc.reason) {
				t.Errorf("first result = %+v, want status %s and reason containing %q", first, tc.status, tc.reason)
			}
			if calls.Load() != 1 || !second.Cached {
				t.Errorf("outcome was retried: calls=%d second=%+v", calls.Load(), second)
			}
		})
	}
}

func TestDeepProbeSingleFlight(t *testing.T) {
	m := New(t.TempDir())
	ci := testConn()
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	m.deepRun = func(_ context.Context, _ *ConnInfo, command string) deepCommandResult {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		marker := strings.Fields(command)[1]
		return deepCommandResult{Stdout: []byte(marker + " PCTOS=%OS% PCTCOMSPEC=%COMSPEC% PSOS=:OS PSCOMSPEC=:ComSpec SH=/bin/sh\n"), Exit: 0}
	}

	var wg sync.WaitGroup
	results := make([]DeepProbeResult, 2)
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		results[0], errs[0] = m.DeepProbe(context.Background(), ci, false)
	}()
	<-started
	go func() {
		defer wg.Done()
		results[1], errs[1] = m.DeepProbe(context.Background(), ci, false)
	}()
	close(release)
	wg.Wait()

	if calls.Load() != 1 {
		t.Fatalf("concurrent probes opened %d sessions, want 1", calls.Load())
	}
	for i := range results {
		if errs[i] != nil || results[i].Status != DeepProbeIdentified || results[i].Dialect != DialectPosix {
			t.Errorf("result %d = %+v, err=%v", i, results[i], errs[i])
		}
	}
}
