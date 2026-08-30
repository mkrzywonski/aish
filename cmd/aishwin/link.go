//go:build windows

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"ai-ssh/internal/aishwinwire"
)

// cmdD is the persistent shell/background-task dispatcher, constructed
// once by main and outliving individual reconnects — a transient WSL/ssh
// hiccup shouldn't lose the shell's cwd state or a running background task.
// Package-level (rather than threaded through run/runOnce as a parameter)
// since menu.go also needs to reach it, to push a live env var into the
// running shell and to report its kind in status output.
var cmdD *commandDispatcher

// run drives the connection to the Linux half for the lifetime of the
// process: spawn, handshake, serve until the link drops, then relaunch with
// backoff. Returns only once ctx is canceled. Takes no session name --
// runOnce reads CurrentSessionName() fresh on each iteration, so a rename
// made between automatic retries is picked up by the very next one.
func run(ctx context.Context, spawn spawnFunc) error {
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	debugLog("run: starting connection loop")

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
	debugLog("runOnce: spawning linux half")
	cmd, stdin, childOut, err := spawn(ctx)
	if err != nil {
		return fmt.Errorf("launching linux half: %w", err)
	}
	debugLog("runOnce: linux half spawned, pid=%d", cmd.Process.Pid)
	defer stdin.Close()

	wc := aishwinwire.NewConn(childOut, stdin)

	hello, err := json.Marshal(aishwinwire.HelloData{
		Proto:           aishwinwire.ProtoVersion,
		Name:            CurrentSessionName(),
		AvailableShells: shellKindStrings(cmdD.available),
		DefaultShell:    string(cmdD.defaultKind),
	})
	if err != nil {
		return err
	}
	debugLog("runOnce: sending hello frame (name=%q, shells=%v)", CurrentSessionName(), shellKindStrings(cmdD.available))
	if debugEnabled.Load() {
		aishwinwire.DebugLog = func(format string, args ...any) {
			debugLog(format, args...)
		}
	}
	if err := wc.Send(aishwinwire.Frame{Type: "hello", Data: hello}); err != nil {
		return fmt.Errorf("sending hello: %w", err)
	}

	debugLog("runOnce: waiting for hello_ack...")
	ackFrame, err := wc.ReadOne()
	if err != nil {
		debugLog("runOnce: hello_ack read error: %v", err)
		return fmt.Errorf("waiting for hello_ack: %w", err)
	}
	if ackFrame.Type != "hello_ack" {
		debugLog("runOnce: expected hello_ack, got %q", ackFrame.Type)
		return fmt.Errorf("expected hello_ack, got %q", ackFrame.Type)
	}
	var ack aishwinwire.HelloAckData
	if err := json.Unmarshal(ackFrame.Data, &ack); err != nil {
		return fmt.Errorf("malformed hello_ack: %w", err)
	}
	debugLog("runOnce: got hello_ack session_id=%s name=%q version=%s", ack.SessionID, ack.Name, ack.Version)
	rt.setConnected(wc, ack)

	AppendLog(fmt.Sprintf("aishwin: connected — session %s is now visible to the AI", sessionLabel(ack)))

	readErr := wc.ReadLoop(func(f aishwinwire.Frame) {
		handleFrame(wc, f)
	})

	debugLog("runOnce: readLoop exited, readErr=%v", readErr)
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
	debugLog("handleFrame: type=%q id=%q dataLen=%d", f.Type, f.ID, len(f.Data))
	switch f.Type {
	case "prompt":
		debugLog("handleFrame: dispatching prompt id=%q", f.ID)
		handlePrompt(wc, f)
	case "notify":
		handleNotify(f)
	case "rename_result":
		// Delivered to the pending Await() registered by menuRename; nothing
		// to do here. Listed for clarity — ReadLoop already routes it there
		// before this function ever sees it.
	case "run_command", "task_poll":
		// Dispatched off the read loop: a foreground command can legitimately
		// run for the caller's full timeout, and must not block prompt/notify
		// frames (or other run_command/task_poll/file_* frames) arriving meanwhile.
		go cmdD.handle(wc, f)
	case "file_read":
		go handleFileRead(wc, f)
	case "file_write":
		go handleFileWrite(wc, f)
	case "file_stat":
		go handleFileStat(wc, f)
	case "directory_list":
		go handleDirectoryList(wc, f)
	case "directory_create":
		go handleDirectoryCreate(wc, f)
	case "file_grep":
		go handleGrep(wc, f)
	case "file_search":
		go handleSearch(wc, f)
	case "capture_screen":
		go handleCaptureScreen(wc, f)
	case "console_read":
		go handleConsoleRead(wc, f)
	default:
		debugLog("handleFrame: unrecognized frame type=%q id=%q", f.Type, f.ID)
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
	debugLog("handlePrompt: id=%q question=%q timeout=%ds -- showing dialog", f.ID, p.Question, p.TimeoutSeconds)
	answer := "n"
	if AskYesNo(p.Question, p.TimeoutSeconds) {
		answer = "y"
	}
	debugLog("handlePrompt: id=%q user answered %q -- sending prompt_answer", f.ID, answer)
	data, err := json.Marshal(aishwinwire.PromptAnswerData{Answer: answer})
	if err != nil {
		debugLog("handlePrompt: id=%q marshal error: %v", f.ID, err)
		return
	}
	debugLog("handlePrompt: id=%q about to Send frame bytes: %s", f.ID, string(mustMarshal(aishwinwire.Frame{Type: "prompt_answer", ID: f.ID, Data: data})))
	if err := wc.Send(aishwinwire.Frame{Type: "prompt_answer", ID: f.ID, Data: data}); err != nil {
		debugLog("handlePrompt: id=%q send error: %v", f.ID, err)
	} else {
		debugLog("handlePrompt: id=%q prompt_answer sent successfully", f.ID)
	}
}
