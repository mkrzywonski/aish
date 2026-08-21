package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"ai-ssh/internal/aicmdwire"
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
// backoff. Returns only once ctx is canceled.
func run(ctx context.Context, spawn spawnFunc, name string) error {
	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for ctx.Err() == nil {
		err := runOnce(ctx, spawn, name)
		rt.setDisconnected()
		if ctx.Err() != nil {
			return nil
		}
		fmt.Fprintf(stderr, "aicmd: linux side disconnected (%v) — retrying in %s\n", err, backoff)
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
func runOnce(ctx context.Context, spawn spawnFunc, name string) error {
	cmd, stdin, childOut, err := spawn(ctx)
	if err != nil {
		return fmt.Errorf("launching linux half: %w", err)
	}
	defer stdin.Close()

	wc := aicmdwire.NewConn(childOut, stdin)

	hello, err := json.Marshal(aicmdwire.HelloData{Proto: aicmdwire.ProtoVersion, Name: name, Shell: string(execD.kind)})
	if err != nil {
		return err
	}
	if err := wc.Send(aicmdwire.Frame{Type: "hello", Data: hello}); err != nil {
		return fmt.Errorf("sending hello: %w", err)
	}

	ackFrame, err := wc.ReadOne()
	if err != nil {
		return fmt.Errorf("waiting for hello_ack: %w", err)
	}
	if ackFrame.Type != "hello_ack" {
		return fmt.Errorf("expected hello_ack, got %q", ackFrame.Type)
	}
	var ack aicmdwire.HelloAckData
	if err := json.Unmarshal(ackFrame.Data, &ack); err != nil {
		return fmt.Errorf("malformed hello_ack: %w", err)
	}
	rt.setConnected(wc, ack)

	fmt.Fprintf(stdout, "aicmd: connected — session %s is now visible to the AI (type 'help' for console commands)\n", sessionLabel(ack))

	readErr := wc.ReadLoop(func(f aicmdwire.Frame) {
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

func sessionLabel(ack aicmdwire.HelloAckData) string {
	if ack.Name != "" {
		return fmt.Sprintf("%s (%s)", ack.SessionID, ack.Name)
	}
	return ack.SessionID
}

func handleFrame(wc *aicmdwire.Conn, f aicmdwire.Frame) {
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
	}
}

func handleNotify(f aicmdwire.Frame) {
	var n aicmdwire.NotifyData
	if err := json.Unmarshal(f.Data, &n); err != nil {
		return
	}
	fmt.Fprintln(stdout, n.Text)
}

func handlePrompt(wc *aicmdwire.Conn, f aicmdwire.Frame) {
	var p aicmdwire.PromptData
	if err := json.Unmarshal(f.Data, &p); err != nil {
		return
	}
	timeout := time.Duration(p.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	answer := askYN(p.Question, timeout)
	data, err := json.Marshal(aicmdwire.PromptAnswerData{Answer: answer})
	if err != nil {
		return
	}
	_ = wc.Send(aicmdwire.Frame{Type: "prompt_answer", ID: f.ID, Data: data})
}
