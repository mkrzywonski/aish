package aicmdd

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"ai-ssh/internal/aicmdwire"
)

// TestExecToolRoundTrip drives aicmdSession.execTool/execStatus directly
// against a fake Windows peer (an io.Pipe, not a real cmd/aicmd process) to
// verify the wire round trip: the frame aicmdd sends, and how it maps a
// synthetic response back into execResult/execStatusResult. Real cmd.exe/
// PowerShell behavior is covered separately by
// cmd/aicmd's TestShellAgainstRealWindowsHost.
func TestExecToolRoundTrip(t *testing.T) {
	peerIn, ourOut := io.Pipe() // aicmdd -> fake peer
	ourIn, peerOut := io.Pipe() // fake peer -> aicmdd
	wire := aicmdwire.NewConn(ourIn, ourOut)
	peer := aicmdwire.NewConn(peerIn, peerOut)
	// ReadLoop is what delivers responses to Await-registered channels in
	// production (it's Run's main blocking loop); without it here, execTool's
	// select on the Await channel would never see the peer's reply.
	go wire.ReadLoop(func(aicmdwire.Frame) {})

	sess := &aicmdSession{id: "test0001", name: "win-test", wire: wire}

	// Fake peer: read the next frame, reply with a canned exec_result.
	go func() {
		f, err := peer.ReadOne()
		if err != nil {
			return
		}
		var req aicmdwire.ExecData
		if err := json.Unmarshal(f.Data, &req); err != nil {
			return
		}
		if req.Command != "echo hi" {
			t.Errorf("peer received command %q, want %q", req.Command, "echo hi")
		}
		code := 0
		data, _ := json.Marshal(aicmdwire.ExecResultData{Output: "hi", ExitCode: &code})
		_ = peer.Send(aicmdwire.Frame{Type: "exec_result", ID: f.ID, Data: data})
	}()

	res, execResult, err := sess.execTool(context.Background(), nil, execArgs{Command: "echo hi"})
	if err != nil {
		t.Fatalf("execTool: %v", err)
	}
	if res != nil {
		t.Errorf("execTool returned a non-nil *CallToolResult for a success case: %#v", res)
	}
	if execResult.Output != "hi" {
		t.Errorf("Output = %q, want %q", execResult.Output, "hi")
	}
	if execResult.ExitCode == nil || *execResult.ExitCode != 0 {
		t.Errorf("ExitCode = %v, want 0", execResult.ExitCode)
	}
	if execResult.Via != "aicmd" {
		t.Errorf("Via = %q, want %q", execResult.Via, "aicmd")
	}
	if execResult.Host != "win-test" {
		t.Errorf("Host = %q, want %q (the declared session name)", execResult.Host, "win-test")
	}
}

// TestExecStatusRoundTrip mirrors TestExecToolRoundTrip for the
// exec_status/exec_poll pair.
func TestExecStatusRoundTrip(t *testing.T) {
	peerIn, ourOut := io.Pipe()
	ourIn, peerOut := io.Pipe()
	wire := aicmdwire.NewConn(ourIn, ourOut)
	peer := aicmdwire.NewConn(peerIn, peerOut)
	go wire.ReadLoop(func(aicmdwire.Frame) {})

	sess := &aicmdSession{id: "test0002", wire: wire}

	go func() {
		f, err := peer.ReadOne()
		if err != nil {
			return
		}
		var req aicmdwire.ExecPollData
		if err := json.Unmarshal(f.Data, &req); err != nil {
			return
		}
		if req.TaskID != "task-abc" {
			t.Errorf("peer received task_id %q, want %q", req.TaskID, "task-abc")
		}
		data, _ := json.Marshal(aicmdwire.ExecPollResultData{Running: true, Output: "partial", NextCursor: 7})
		_ = peer.Send(aicmdwire.Frame{Type: "exec_poll_result", ID: f.ID, Data: data})
	}()

	_, statusResult, err := sess.execStatus(context.Background(), nil, execStatusArgs{TaskID: "task-abc"})
	if err != nil {
		t.Fatalf("execStatus: %v", err)
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

// TestExecToolTimesOutCleanly confirms a peer that receives the request but
// never answers surfaces as an error once the wait budget elapses, rather
// than hanging indefinitely. Uses Background: true so it exercises the
// shorter 15s wait budget (see execWaitTimeout) instead of the ~40s
// foreground default.
func TestExecToolTimesOutCleanly(t *testing.T) {
	peerIn, ourOut := io.Pipe()
	ourIn, _ := io.Pipe() // never written to: the peer never sends a reply
	wire := aicmdwire.NewConn(ourIn, ourOut)
	go wire.ReadLoop(func(aicmdwire.Frame) {})

	// Drain (but never answer) the request — without this, Send() would
	// block forever on the unread pipe instead of the intended no-response
	// timeout ever being reached.
	go func() { _, _ = io.Copy(io.Discard, peerIn) }()

	sess := &aicmdSession{id: "test0003", wire: wire}

	done := make(chan error, 1)
	go func() {
		_, _, err := sess.execTool(context.Background(), nil, execArgs{Command: "echo hi", Background: true})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Error("expected an error when the Windows peer never responds")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("execTool did not return within the background wait budget")
	}
}
