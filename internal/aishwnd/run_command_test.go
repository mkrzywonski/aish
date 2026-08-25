package aishwnd

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"ai-ssh/internal/aishwinwire"
)

// TestRunCommandRoundTrip drives aishwndSession.runCommandTool/taskStatus directly
// against a fake Windows peer (an io.Pipe, not a real cmd/aishwin process) to
// verify the wire round trip: the frame aishwnd sends, and how it maps a
// synthetic response back into runCommandResult/taskStatusResult. Real cmd.exe/
// PowerShell behavior is covered separately by
// cmd/aishwin's TestShellAgainstRealWindowsHost.
func TestRunCommandRoundTrip(t *testing.T) {
	peerIn, ourOut := io.Pipe() // aishwnd -> fake peer
	ourIn, peerOut := io.Pipe() // fake peer -> aishwnd
	wire := aishwinwire.NewConn(ourIn, ourOut)
	peer := aishwinwire.NewConn(peerIn, peerOut)
	// ReadLoop is what delivers responses to Await-registered channels in
	// production (it's Run's main blocking loop); without it here, runCommandTool's
	// select on the Await channel would never see the peer's reply.
	go wire.ReadLoop(func(aishwinwire.Frame) {})

	sess := &aishwndSession{id: "test0001", name: "win-test", wire: wire}

	// Fake peer: read the next frame, reply with a canned exec_result.
	go func() {
		f, err := peer.ReadOne()
		if err != nil {
			return
		}
		var req aishwinwire.RunCommandData
		if err := json.Unmarshal(f.Data, &req); err != nil {
			return
		}
		if req.Command != "echo hi" {
			t.Errorf("peer received command %q, want %q", req.Command, "echo hi")
		}
		code := 0
		data, _ := json.Marshal(aishwinwire.RunCommandResultData{Output: "hi", ExitCode: &code})
		_ = peer.Send(aishwinwire.Frame{Type: "run_command_result", ID: f.ID, Data: data})
	}()

	res, runCommandResult, err := sess.runCommandTool(context.Background(), nil, runCommandArgs{Command: "echo hi"})
	if err != nil {
		t.Fatalf("runCommandTool: %v", err)
	}
	if res != nil {
		t.Errorf("runCommandTool returned a non-nil *CallToolResult for a success case: %#v", res)
	}
	if runCommandResult.Output != "hi" {
		t.Errorf("Output = %q, want %q", runCommandResult.Output, "hi")
	}
	if runCommandResult.ExitCode == nil || *runCommandResult.ExitCode != 0 {
		t.Errorf("ExitCode = %v, want 0", runCommandResult.ExitCode)
	}
	if runCommandResult.Via != "aishwin" {
		t.Errorf("Via = %q, want %q", runCommandResult.Via, "aishwin")
	}
	if runCommandResult.Host != "win-test" {
		t.Errorf("Host = %q, want %q (the declared session name)", runCommandResult.Host, "win-test")
	}
}

// TestTaskStatusRoundTrip mirrors TestRunCommandRoundTrip for the
// task_status/task_poll pair.
func TestTaskStatusRoundTrip(t *testing.T) {
	peerIn, ourOut := io.Pipe()
	ourIn, peerOut := io.Pipe()
	wire := aishwinwire.NewConn(ourIn, ourOut)
	peer := aishwinwire.NewConn(peerIn, peerOut)
	go wire.ReadLoop(func(aishwinwire.Frame) {})

	sess := &aishwndSession{id: "test0002", wire: wire}

	go func() {
		f, err := peer.ReadOne()
		if err != nil {
			return
		}
		var req aishwinwire.TaskPollData
		if err := json.Unmarshal(f.Data, &req); err != nil {
			return
		}
		if req.TaskID != "task-abc" {
			t.Errorf("peer received task_id %q, want %q", req.TaskID, "task-abc")
		}
		data, _ := json.Marshal(aishwinwire.TaskPollResultData{Running: true, Output: "partial", NextCursor: 7})
		_ = peer.Send(aishwinwire.Frame{Type: "task_poll_result", ID: f.ID, Data: data})
	}()

	_, statusResult, err := sess.taskStatus(context.Background(), nil, taskStatusArgs{TaskID: "task-abc"})
	if err != nil {
		t.Fatalf("taskStatus: %v", err)
	}
	if !statusResult.Running {
		t.Error("Running = false, want true")
	}
	if statusResult.Output != "partial" {
		t.Errorf("Output = %q, want %q", statusResult.Output, "partial")
	}
	if statusResult.NextCursor != 7 {
		t.Errorf("NextCursor = %d, want 7", statusResult.NextCursor)
	}
}

// TestRunCommandTimesOutCleanly confirms a peer that receives the request but
// never answers surfaces as an error once the wait budget elapses, rather
// than hanging indefinitely. Uses Background: true so it exercises the
// shorter 15s wait budget (see runCommandWaitTimeout) instead of the ~40s
// foreground default.
func TestRunCommandTimesOutCleanly(t *testing.T) {
	peerIn, ourOut := io.Pipe()
	ourIn, _ := io.Pipe() // never written to: the peer never sends a reply
	wire := aishwinwire.NewConn(ourIn, ourOut)
	go wire.ReadLoop(func(aishwinwire.Frame) {})

	// Drain (but never answer) the request — without this, Send() would
	// block forever on the unread pipe instead of the intended no-response
	// timeout ever being reached.
	go func() { _, _ = io.Copy(io.Discard, peerIn) }()

	sess := &aishwndSession{id: "test0003", wire: wire}

	done := make(chan error, 1)
	go func() {
		_, _, err := sess.runCommandTool(context.Background(), nil, runCommandArgs{Command: "echo hi", Background: true})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Error("expected an error when the Windows peer never responds")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("runCommandTool did not return within the background wait budget")
	}
}
