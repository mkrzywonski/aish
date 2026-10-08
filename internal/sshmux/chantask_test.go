package sshmux

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// channelTaskHarness runs the persistent channel against a local `sh -s`
// through a fake ssh that logs every invocation, so tests can assert that
// launching and polling tasks opens no session beyond the channel itself.
type channelTaskHarness struct {
	t       *testing.T
	m       *Mux
	ci      *ConnInfo
	opens   string
	session string
}

var harnessSeq atomic.Int64

func newChannelTaskHarness(t *testing.T) *channelTaskHarness {
	return newChannelTaskHarnessWithPath(t, "")
}

// newChannelTaskHarnessWithPath puts binDir first on the channel shell's
// PATH, so tests can substitute broken utilities (e.g. setsid).
func newChannelTaskHarnessWithPath(t *testing.T, binDir string) *channelTaskHarness {
	t.Helper()
	dir := t.TempDir()
	opens := filepath.Join(dir, "opens")
	fake := filepath.Join(dir, "fake-ssh")
	pathSetup := ""
	if binDir != "" {
		pathSetup = "PATH=" + Quote(binDir) + ":$PATH; export PATH\n"
	}
	script := fmt.Sprintf("#!/bin/sh\necho open >> %s\n%sexec sh -s\n", Quote(opens), pathSetup)
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	m := New(dir)
	m.realSSH = fake
	h := &channelTaskHarness{
		t: t, m: m, ci: testConn(), opens: opens,
		session: fmt.Sprintf("test%d-%d", os.Getpid(), harnessSeq.Add(1)),
	}
	t.Cleanup(func() {
		m.closeChannels()
		matches, _ := filepath.Glob("/tmp/aish-task-" + h.session + "-*")
		for _, p := range matches {
			os.RemoveAll(p)
		}
	})
	return h
}

func (h *channelTaskHarness) openCount() int {
	b, _ := os.ReadFile(h.opens)
	return bytes.Count(b, []byte("open"))
}

func (h *channelTaskHarness) launch(id, command, cwd string) *ChannelTask {
	h.t.Helper()
	dir, err := newChannelTaskDir(h.session, id)
	if err != nil {
		h.t.Fatal(err)
	}
	ct := &ChannelTask{CI: *h.ci, Dir: dir}
	if err := h.m.LaunchChannelTask(ct, command, cwd); err != nil {
		h.t.Fatalf("launch %q: %v", command, err)
	}
	return ct
}

func terminal(state string) bool {
	switch state {
	case TaskDone, TaskCaptureFailed, TaskLost, TaskExpired:
		return true
	}
	return false
}

// drain polls ct from cursor 0 until it reaches a terminal state, returning
// the concatenated output and the final status.
func (h *channelTaskHarness) drain(ct *ChannelTask, max int) ([]byte, ChannelTaskStatus) {
	h.t.Helper()
	var out []byte
	cursor := int64(0)
	deadline := time.Now().Add(10 * time.Second)
	for {
		st, err := h.m.PollChannelTask(ct, cursor, max)
		if err != nil {
			h.t.Fatalf("poll: %v", err)
		}
		out = append(out, st.Output...)
		cursor = st.Next
		if terminal(st.State) && len(st.Output) == 0 {
			return out, st
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("task did not finish; last state %q", st.State)
		}
		if len(st.Output) == 0 {
			time.Sleep(20 * time.Millisecond)
		}
	}
}

func TestChannelTaskLifecycleOpensNoExtraSession(t *testing.T) {
	h := newChannelTaskHarness(t)
	ct := h.launch("task-1", "printf 'one\\n'; sleep 0.2; echo two >&2; exit 3", "")
	out, st := h.drain(ct, 4)
	if string(out) != "one\ntwo\n" {
		t.Errorf("output = %q", out)
	}
	if st.State != TaskDone || st.ExitCode == nil || *st.ExitCode != 3 {
		t.Errorf("final status = %+v", st)
	}
	if n := h.openCount(); n != 1 {
		t.Errorf("ssh invoked %d times; launch and polls must reuse the one channel", n)
	}
}

func TestChannelTaskWorksOnWarmChannelWhileNewSessionsBlocked(t *testing.T) {
	h := newChannelTaskHarness(t)
	if _, err := h.m.EnsureProbed(h.ci); err != nil {
		t.Fatal(err)
	}
	h.m.SetBlockNewSessions(true)
	ct := h.launch("task-1", "echo ok", "")
	if out, st := h.drain(ct, 1024); string(out) != "ok\n" || st.State != TaskDone {
		t.Errorf("output %q, status %+v", out, st)
	}
	if n := h.openCount(); n != 1 {
		t.Errorf("ssh invoked %d times with new sessions blocked", n)
	}
}

func TestChannelTaskCommandCannotBypassSupervisor(t *testing.T) {
	h := newChannelTaskHarness(t)
	for i, tc := range []struct {
		cmd  string
		exit int
		out  string
	}{
		{"echo before; exit 7; echo after", 7, "before\n"},
		{"exec sh -c 'echo replaced; exit 5'", 5, "replaced\n"},
		{"printf '%s\\n' \"it's 'quoted'\"\necho \"second line\"", 0, "it's 'quoted'\nsecond line\n"},
	} {
		ct := h.launch(fmt.Sprintf("task-%d", i+1), tc.cmd, "")
		out, st := h.drain(ct, 1024)
		if string(out) != tc.out || st.State != TaskDone || st.ExitCode == nil || *st.ExitCode != tc.exit {
			t.Errorf("%q: output %q, status %+v", tc.cmd, out, st)
		}
	}
	// The shared shell must be unaffected by everything above.
	res, err := h.m.ChannelRun(h.ci, "echo alive", time.Second)
	if err != nil || strings.TrimSpace(string(res.Output)) != "alive" {
		t.Fatalf("channel unusable after tasks: %v %+v", err, res)
	}
}

func TestChannelTaskBinaryOutputAndCursors(t *testing.T) {
	h := newChannelTaskHarness(t)
	// Bytes that look like protocol metadata, a NUL, and no trailing newline.
	want := "AISHPOLL ok exit=0\n@AISH@x@0@\n\x00\x01\xff end"
	ct := h.launch("task-1", `printf 'AISHPOLL ok exit=0\n@AISH@x@0@\n\000\001\377 end'`, "")
	out, _ := h.drain(ct, 5)
	if string(out) != want {
		t.Fatalf("output = %q, want %q", out, want)
	}
	// Negative cursor: the last n bytes.
	st, err := h.m.PollChannelTask(ct, -1, 4)
	if err != nil || string(st.Output) != " end" || st.Next != int64(len(want)) {
		t.Errorf("tail read: %q next=%d err=%v", st.Output, st.Next, err)
	}
	// A cursor past the end clamps to the end.
	st, err = h.m.PollChannelTask(ct, 1<<30, 4)
	if err != nil || len(st.Output) != 0 || st.Next != int64(len(want)) {
		t.Errorf("future cursor: %q next=%d err=%v", st.Output, st.Next, err)
	}
	// Repeat reads of the same cursor return the same bytes.
	a, _ := h.m.PollChannelTask(ct, 3, 6)
	b, _ := h.m.PollChannelTask(ct, 3, 6)
	if string(a.Output) != want[3:9] || string(b.Output) != string(a.Output) {
		t.Errorf("repeat reads %q / %q", a.Output, b.Output)
	}
}

func TestChannelTaskCwd(t *testing.T) {
	h := newChannelTaskHarness(t)
	dir := t.TempDir()
	real, _ := filepath.EvalSymlinks(dir)
	ct := h.launch("task-1", "pwd -P", dir)
	if out, st := h.drain(ct, 1024); strings.TrimSpace(string(out)) != real || st.State != TaskDone {
		t.Errorf("pwd output %q, status %+v", out, st)
	}
	ct = h.launch("task-2", "echo should-not-run", filepath.Join(dir, "missing"))
	out, st := h.drain(ct, 1024)
	if !st.CwdFailed || st.ExitCode == nil || *st.ExitCode == 0 || strings.Contains(string(out), "should-not-run") {
		t.Errorf("bad cwd: output %q, status %+v", out, st)
	}
}

func TestChannelTaskOutputLimitBoundaries(t *testing.T) {
	h := newChannelTaskHarness(t)
	for i, n := range []int{0, ChannelTaskOutputLimit - 1, ChannelTaskOutputLimit, ChannelTaskOutputLimit + 1, ChannelTaskOutputLimit + 4096} {
		ct := h.launch(fmt.Sprintf("task-%d", i+1), fmt.Sprintf("head -c %d /dev/zero", n), "")
		var st ChannelTaskStatus
		deadline := time.Now().Add(10 * time.Second)
		for {
			var err error
			st, err = h.m.PollChannelTask(ct, -1, 16)
			if err != nil {
				t.Fatal(err)
			}
			if terminal(st.State) || time.Now().After(deadline) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		wantNext := int64(n)
		if wantNext > ChannelTaskOutputLimit {
			wantNext = ChannelTaskOutputLimit
		}
		if st.State != TaskDone || st.Next != wantNext || st.OutputLimited != (n > ChannelTaskOutputLimit) {
			t.Errorf("%d bytes: status %+v (next %d, want %d)", n, st, st.Next, wantNext)
		}
	}
}

func TestChannelTaskDrainingWhileDescendantHoldsOutput(t *testing.T) {
	h := newChannelTaskHarness(t)
	ct := h.launch("task-1", "(sleep 1; echo late) & echo early", "")
	deadline := time.Now().Add(5 * time.Second)
	sawDraining := false
	for {
		st, err := h.m.PollChannelTask(ct, 0, 1024)
		if err != nil {
			t.Fatal(err)
		}
		if st.State == TaskDraining {
			sawDraining = true
		}
		if st.State == TaskDone {
			if string(st.Output) != "early\nlate\n" {
				t.Errorf("final output %q", st.Output)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stuck in %q", st.State)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !sawDraining {
		t.Error("never observed draining while the background child held the output open")
	}
}

func TestChannelTaskLostAndExpired(t *testing.T) {
	h := newChannelTaskHarness(t)
	ct := h.launch("task-1", "sleep 3", "")
	var pid []byte
	deadline := time.Now().Add(5 * time.Second)
	for len(pid) == 0 {
		pid, _ = os.ReadFile(filepath.Join(ct.Dir, "started"))
		if time.Now().After(deadline) {
			t.Fatal("supervisor never published started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := exec.Command("kill", "-9", strings.TrimSpace(string(pid))).Run(); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		st, err := h.m.PollChannelTask(ct, 0, 16)
		if err != nil {
			t.Fatal(err)
		}
		if st.State == TaskLost {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("killed supervisor reported %q, want lost", st.State)
		}
		time.Sleep(20 * time.Millisecond)
	}
	os.RemoveAll(ct.Dir)
	if st, err := h.m.PollChannelTask(ct, 0, 16); err != nil || st.State != TaskExpired {
		t.Errorf("removed dir: %+v %v", st, err)
	}
}

func TestChannelTaskLaunchFailureLeavesShellUsable(t *testing.T) {
	h := newChannelTaskHarness(t)
	ct := &ChannelTask{CI: *h.ci, Dir: "/nonexistent-aish-parent/task"}
	err := h.m.LaunchChannelTask(ct, "echo hi", "")
	if err == nil || strings.Contains(err.Error(), ErrTaskLaunchUncertain.Error()) {
		t.Fatalf("mkdir failure should be a definite error, got %v", err)
	}
	res, err := h.m.ChannelRun(h.ci, "echo alive", time.Second)
	if err != nil || strings.TrimSpace(string(res.Output)) != "alive" {
		t.Fatalf("channel unusable after failed launch: %v %+v", err, res)
	}
}

func TestParseChannelTaskPollUncertainAndMalformed(t *testing.T) {
	if st, err := parseChannelTaskPoll([]byte("AISHPOLL nodir\n"), false); err != nil || st.State != TaskUncertain {
		t.Errorf("unsubmitted missing dir: %+v %v", st, err)
	}
	if st, err := parseChannelTaskPoll([]byte("AISHPOLL nodir\n"), true); err != nil || st.State != TaskExpired {
		t.Errorf("submitted missing dir: %+v %v", st, err)
	}
	for _, bad := range []string{
		"garbage\n",
		"AISHPOLL ok pid=1 alive=1 rc=- cdf=0 size=x start=0 count=0 exit=-\n",
		"AISHPOLL ok pid=1 alive=1 rc=- cdf=0 size=5 start=0 count=5 exit=-\naGk=\n", // 2 bytes, not 5
		"AISHPOLL ok pid=1 alive=- rc=0 cdf=0 size=0 start=0 count=0 exit=0,0\n",
	} {
		if _, err := parseChannelTaskPoll([]byte(bad), true); err == nil {
			t.Errorf("accepted malformed poll %q", bad)
		}
	}
	st, err := parseChannelTaskPoll([]byte("AISHPOLL ok pid=1 alive=- rc=0 cdf=0 size=0 start=0 count=0 exit=0,1,0\n"), true)
	if err != nil || st.State != TaskCaptureFailed || st.ExitCode == nil || *st.ExitCode != 0 {
		t.Errorf("capture failure: %+v %v", st, err)
	}
}

// A task with no cwd must run where foreground exec does (the channel
// shell's directory), not wherever the supervisor happens to be.
func TestChannelTaskDefaultCwdMatchesForeground(t *testing.T) {
	h := newChannelTaskHarness(t)
	fg, err := h.m.ChannelRun(h.ci, "pwd -P", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ct := h.launch("task-1", "pwd -P", "")
	out, st := h.drain(ct, 1024)
	if st.State != TaskDone || strings.TrimSpace(string(out)) != strings.TrimSpace(string(fg.Output)) {
		t.Errorf("background cwd %q, foreground cwd %q (status %+v)", out, fg.Output, st)
	}
}

// cwd must reach cd byte-for-byte: a trailing newline is part of the path.
func TestChannelTaskCwdWithTrailingNewline(t *testing.T) {
	h := newChannelTaskHarness(t)
	base := filepath.Join(t.TempDir(), "job")
	for _, d := range []string{base, base + "\n"} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	ct := h.launch("task-1", `printf '[%s]' "$PWD"`, base+"\n")
	out, st := h.drain(ct, 1024)
	if st.State != TaskDone || string(out) != "["+base+"\n]" {
		t.Errorf("ran in %q, want %q (status %+v)", out, "["+base+"\n]", st)
	}
}

// A detacher that fails before the supervisor starts must surface as
// start_failed, not sit in "starting" forever.
func TestChannelTaskDetacherFailureIsReported(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "setsid"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	h := newChannelTaskHarnessWithPath(t, bin)
	ct := h.launch("task-1", "echo should-not-run", "")
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, err := h.m.PollChannelTask(ct, 0, 1024)
		if err != nil {
			t.Fatal(err)
		}
		if st.State == TaskStartFailed {
			if st.StartFailure != "1" || len(st.Output) != 0 {
				t.Errorf("start failure status %+v", st)
			}
			return
		}
		if st.State != TaskStarting || time.Now().After(deadline) {
			t.Fatalf("broken setsid reported %q, want start_failed", st.State)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Output removed after completion must not read as a successful empty
// capture, and must not rewind the caller's cursor.
func TestChannelTaskMissingOutputAfterCompletion(t *testing.T) {
	h := newChannelTaskHarness(t)
	ct := h.launch("task-1", "echo hello world", "")
	if out, st := h.drain(ct, 1024); string(out) != "hello world\n" || st.State != TaskDone {
		t.Fatalf("output %q, status %+v", out, st)
	}
	if err := os.Remove(filepath.Join(ct.Dir, "out")); err != nil {
		t.Fatal(err)
	}
	st, err := h.m.PollChannelTask(ct, 5, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != TaskCaptureFailed || !st.OutputMissing || st.Next != 5 || st.ExitCode == nil || *st.ExitCode != 0 {
		t.Errorf("missing output reported as %+v", st)
	}
}
