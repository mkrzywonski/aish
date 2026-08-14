package sshmux

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSFTPPathStyle(t *testing.T) {
	tests := map[string]string{
		"/home/mike":       "posix",
		"/Users/mike":      "posix",
		"/C:/Users/mk31":   "windows",
		"/d:/work/project": "windows",
		"relative/path":    "unknown",
		"":                 "unknown",
	}
	for path, want := range tests {
		if got := sftpPathStyle(path); got != want {
			t.Errorf("sftpPathStyle(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestSubsystemCommandUsesExistingMaster(t *testing.T) {
	m := New(t.TempDir())
	ci := &ConnInfo{Sock: "/run/aish/cm-test", Host: "server", User: "mike", Port: "2222"}
	got := m.SubsystemCommand(ci, "sftp").Args
	want := []string{m.realSSH, "-S", ci.Sock, "-oControlMaster=no", "-oBatchMode=yes", "-p", "2222", "-l", "mike", "-s", "server", "sftp"}
	if !slices.Equal(got, want) {
		t.Errorf("subsystem command = %q, want %q", got, want)
	}
}

func TestRunSFTPProbeTimesOutAndReapsProcess(t *testing.T) {
	m := New(t.TempDir())
	m.sftpTimeout = 20 * time.Millisecond
	script := filepath.Join(t.TempDir(), "fake-ssh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	m.realSSH = script

	started := time.Now()
	session, axis := m.runSFTPProbe(context.Background(), testConn())
	if session != nil {
		t.Fatal("timed-out probe retained a session")
	}
	if axis.State != AxisDown || !strings.Contains(axis.Reason, "timed out") {
		t.Errorf("timeout result = %+v", axis)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("timeout took %s, want under 1s", elapsed)
	}
}

func TestRunSFTPProbeCancellationIsClassifiedAndReaped(t *testing.T) {
	m := New(t.TempDir())
	m.sftpTimeout = time.Second
	script := filepath.Join(t.TempDir(), "fake-ssh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	m.realSSH = script
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	started := time.Now()
	session, axis := m.runSFTPProbe(ctx, testConn())
	if session != nil {
		t.Fatal("canceled probe retained a session")
	}
	if axis.State != AxisDown || !strings.Contains(axis.Reason, "canceled during handshake") {
		t.Errorf("canceled result = %+v", axis)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("cancellation took %s, want under 1s", elapsed)
	}
}

func TestSFTPProbeCachesSuccessAndIdentity(t *testing.T) {
	m := New(t.TempDir())
	ci := testConn()
	var calls atomic.Int32
	m.sftpRun = func(context.Context, *ConnInfo) (*sftpSession, SftpAxis) {
		calls.Add(1)
		return nil, SftpAxis{
			State: AxisUp, RealPath: "/C:/Users/mk31", PathStyle: "windows",
			Extensions: []string{"posix-rename@openssh.com"}, ProbedAt: time.Now(),
		}
	}

	first, err := m.ProbeSFTP(context.Background(), ci, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.ProbeSFTP(context.Background(), ci, false)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || first.Cached || !second.Cached {
		t.Errorf("cache behavior: calls=%d first=%+v second=%+v", calls.Load(), first, second)
	}
	if first.Axis.State != AxisUp || first.Axis.Attempts != 1 || first.Axis.PathStyle != "windows" {
		t.Errorf("first result = %+v", first)
	}

	f, ok := m.Facts(ci)
	if !ok {
		t.Fatal("SFTP probe did not create durable facts")
	}
	if f.Shell.State != AxisUnknown {
		t.Errorf("SFTP probe changed shell state to %s", f.Shell.State)
	}
	platform := f.Identity.PlatformFact()
	if platform.Platform != "windows" || platform.Source != IdentitySourceSFTP {
		t.Errorf("SFTP platform identity = %+v", platform)
	}
	if dialect := f.Identity.DialectFact(); dialect.Dialect != DialectUnknown {
		t.Errorf("SFTP path evidence claimed dialect %q", dialect.Dialect)
	}

	// Facts and cache reads must not expose the stored extension slice.
	f.SFTP.Extensions[0] = "mutated"
	cached, _ := m.CachedSFTPProbe(ci)
	if cached.Extensions[0] != "posix-rename@openssh.com" {
		t.Errorf("cached extensions were mutated through a facts copy: %q", cached.Extensions)
	}
}

func TestSFTPProbeCachesFailure(t *testing.T) {
	m := New(t.TempDir())
	ci := testConn()
	var calls atomic.Int32
	m.sftpRun = func(context.Context, *ConnInfo) (*sftpSession, SftpAxis) {
		calls.Add(1)
		return nil, SftpAxis{State: AxisDown, Reason: "subsystem request failed", ProbedAt: time.Now()}
	}

	first, err := m.ProbeSFTP(context.Background(), ci, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.ProbeSFTP(context.Background(), ci, false)
	if err != nil {
		t.Fatal(err)
	}
	if first.Axis.State != AxisDown || !strings.Contains(first.Axis.Reason, "failed") {
		t.Errorf("failed result = %+v", first)
	}
	if calls.Load() != 1 || !second.Cached {
		t.Errorf("failed result retried: calls=%d second=%+v", calls.Load(), second)
	}
}

func TestSFTPProbeNormalizesEmptyRunnerResult(t *testing.T) {
	m := New(t.TempDir())
	ci := testConn()
	m.sftpRun = func(context.Context, *ConnInfo) (*sftpSession, SftpAxis) {
		return nil, SftpAxis{}
	}

	first, err := m.ProbeSFTP(context.Background(), ci, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.ProbeSFTP(context.Background(), ci, false)
	if err != nil {
		t.Fatal(err)
	}
	if first.Axis.State != AxisDown || first.Axis.ProbedAt.IsZero() || !strings.Contains(first.Axis.Reason, "no capability state") {
		t.Errorf("normalized result = %+v", first)
	}
	if !second.Cached || second.Axis.Attempts != 1 {
		t.Errorf("normalized failure was not cached: %+v", second)
	}
}

func TestSFTPProbeForceResetsOnlySFTP(t *testing.T) {
	m := New(t.TempDir())
	ci := testConn()
	m.NoteShellUsable(ci, Capabilities{Hostname: "buildbox"})
	m.NoteIdentity(ci, DialectPowerShell, "windows", IdentitySourceDeepProbe, "deep evidence")
	var calls atomic.Int32
	m.sftpRun = func(context.Context, *ConnInfo) (*sftpSession, SftpAxis) {
		calls.Add(1)
		return nil, SftpAxis{State: AxisUp, RealPath: "/home/mike", PathStyle: "posix", ProbedAt: time.Now()}
	}

	if _, err := m.ProbeSFTP(context.Background(), ci, false); err != nil {
		t.Fatal(err)
	}
	forced, err := m.ProbeSFTP(context.Background(), ci, true)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || forced.Cached || forced.Axis.Attempts != 1 {
		t.Errorf("forced result = %+v, calls=%d", forced, calls.Load())
	}
	f, _ := m.Facts(ci)
	if !f.ShellUsable() || f.Shell.Caps.Hostname != "buildbox" {
		t.Errorf("SFTP force reset shell facts: %+v", f.Shell)
	}
	if dialect := f.Identity.DialectFact(); dialect.Dialect != DialectPowerShell || dialect.Source != IdentitySourceDeepProbe {
		t.Errorf("SFTP force reset deep identity: %+v", dialect)
	}
}

func TestSFTPProbeSingleFlightAndActivity(t *testing.T) {
	m := New(t.TempDir())
	m.attemptDebounce = 10 * time.Millisecond
	ci := testConn()
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	m.sftpRun = func(context.Context, *ConnInfo) (*sftpSession, SftpAxis) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return nil, SftpAxis{State: AxisUp, RealPath: "/home/mike", PathStyle: "posix", ProbedAt: time.Now()}
	}

	var wg sync.WaitGroup
	results := make([]SFTPProbeResult, 2)
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		results[0], errs[0] = m.ProbeSFTP(context.Background(), ci, false)
	}()
	<-started
	go func() {
		defer wg.Done()
		results[1], errs[1] = m.ProbeSFTP(context.Background(), ci, false)
	}()

	deadline := time.Now().Add(time.Second)
	for {
		if attempt, ok := m.VisibleSessionAttempt(); ok {
			if attempt.Kind != SessionAttemptSFTP || attempt.Count != 1 {
				t.Fatalf("attempt = %+v", attempt)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("SFTP attempt did not become visible")
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()

	if calls.Load() != 1 {
		t.Fatalf("concurrent probes opened %d subsystems, want 1", calls.Load())
	}
	for i := range results {
		if errs[i] != nil || results[i].Axis.State != AxisUp {
			t.Errorf("result %d = %+v, err=%v", i, results[i], errs[i])
		}
	}
	if _, ok := m.VisibleSessionAttempt(); ok {
		t.Fatal("SFTP attempt remained visible after startup completed")
	}
}
