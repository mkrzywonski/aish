package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"ai-ssh/internal/aishwinwire"
)

// execD is the persistent shell/background-task dispatcher, constructed
// once by main and outliving individual reconnects — a transient WSL/ssh
// hiccup shouldn't lose the shell's cwd state or a running background task.
// Package-level (rather than threaded through run/runOnce as a parameter)
// since menu.go also needs to reach it, to push a live env var into the
// running shell and to report its kind in status output.
var execD *execDispatcher

// run drives the connection to the Linux half for the lifetime of the
// process: spawn, handshake, serve until the link drops, then relaunch with
// backoff. Returns only once ctx is canceled. Takes no session name --
// runOnce reads CurrentSessionName() fresh on each iteration, so a rename
// made between automatic retries is picked up by the very next one.
func run(ctx context.Context, spawn spawnFunc) error {
	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for ctx.Err() == nil {
		err := runOnce(ctx, spawn)
		rt.setDisconnected()
		if ctx.Err() != nil {
			return nil
		}
		AppendLog(fmt.Sprintf("aishwin: linux side disconnected (%v) — retrying in %s", err, backoff))
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil
		}
		if backoff < maxBackoff {
			backoff *= 2
		}
	}
	return nil
}

// runOnce spawns one instance of the Linux half, completes the hello
// handshake, and serves frames from it until the link drops.
func runOnce(ctx context.Context, spawn spawnFunc) error {
	cmd, stdin, childOut, err := spawn(ctx)
	if err != nil {
		return fmt.Errorf("launching linux half: %w", err)
	}
	defer stdin.Close()

	wc := aishwinwire.NewConn(childOut, stdin)

	hello, err := json.Marshal(aishwinwire.HelloData{
		Proto:           aishwinwire.ProtoVersion,
		Name:            CurrentSessionName(),
		AvailableShells: shellKindStrings(execD.available),
		DefaultShell:    string(execD.defaultKind),
	})
	if err != nil {
		return err
	}
	if err := wc.Send(aishwinwire.Frame{Type: "hello", Data: hello}); err != nil {
		return fmt.Errorf("sending hello: %w", err)
	}

	ackFrame, err := wc.ReadOne()
	if err != nil {
		return fmt.Errorf("waiting for hello_ack: %w", err)
	}
	if ackFrame.Type != "hello_ack" {
		return fmt.Errorf("expected hello_ack, got %q", ackFrame.Type)
	}
	var ack aishwinwire.HelloAckData
	if err := json.Unmarshal(ackFrame.Data, &ack); err != nil {
		return fmt.Errorf("malformed hello_ack: %w", err)
	}
	rt.setConnected(wc, ack)

	AppendLog(fmt.Sprintf("aishwin: connected — session %s is now visible to the AI", sessionLabel(ack)))

	readErr := wc.ReadLoop(func(f aishwinwire.Frame) {
		handleFrame(wc, f)
	})

	waitErr := cmd.Wait()
	if readErr != nil {
		return readErr
	}
	if waitErr != nil {
		return fmt.Errorf("linux half exited: %w", waitErr)
	}
	return errors.New("linux half exited")
}

func sessionLabel(ack aishwinwire.HelloAckData) string {
	if ack.Name != "" {
		return fmt.Sprintf("%s (%s)", ack.SessionID, ack.Name)
	}
	return ack.SessionID
}

func handleFrame(wc *aishwinwire.Conn, f aishwinwire.Frame) {
	switch f.Type {
	case "prompt":
		handlePrompt(wc, f)
	case "notify":
		handleNotify(f)
	case "rename_result":
		// Delivered to the pending Await() registered by menuRename; nothing
		// to do here. Listed for clarity — ReadLoop already routes it there
		// before this function ever sees it.
	case "exec", "exec_poll":
		// Dispatched off the read loop: a foreground command can legitimately
		// run for the caller's full timeout, and must not block prompt/notify
		// frames (or other exec/exec_poll/file_* frames) arriving meanwhile.
		go execD.handle(wc, f)
	case "file_read":
		go handleFileRead(wc, f)
	case "file_write":
		go handleFileWrite(wc, f)
	case "file_stat":
		go handleFileStat(wc, f)
	case "directory_list":
		go handleDirectoryList(wc, f)
	case "file_grep":
		go handleGrep(wc, f)
	case "file_search":
		go handleSearch(wc, f)
	case "capture_screen":
		go handleCaptureScreen(wc, f)
	}
}

func handleNotify(f aishwinwire.Frame) {
	var n aishwinwire.NotifyData
	if err := json.Unmarshal(f.Data, &n); err != nil {
		return
	}
	AppendLog(n.Text)
}

func handlePrompt(wc *aishwinwire.Conn, f aishwinwire.Frame) {
	var p aishwinwire.PromptData
	if err := json.Unmarshal(f.Data, &p); err != nil {
		return
	}
	answer := "n"
	if AskYesNo(p.Question, p.TimeoutSeconds) {
		answer = "y"
	}
	data, err := json.Marshal(aishwinwire.PromptAnswerData{Answer: answer})
	if err != nil {
		return
	}
	_ = wc.Send(aishwinwire.Frame{Type: "prompt_answer", ID: f.ID, Data: data})
}
