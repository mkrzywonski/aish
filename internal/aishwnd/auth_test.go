package aishwnd

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ai-ssh/internal/aishwinwire"
	"ai-ssh/internal/authproto"
	"ai-ssh/internal/paths"
)

// TestApprovalSurvivesListClientsPoll is the regression test for a deadlock
// found live: requestAccess held the connection's auth mutex while waiting
// for the prompt's answer, and the wire ReadLoop that delivers that answer
// also served aishwin's periodic list_clients poll, which takes the same
// mutex. One poll arriving while a prompt was up blocked the ReadLoop, so
// the user's "yes" was never read and every approval timed out.
//
// The fake Windows peer here behaves like aishwin's client-count poller: on
// receiving a prompt it first asks for the client list, and only answers
// the prompt once that list arrives.
func TestApprovalSurvivesListClientsPoll(t *testing.T) {
	// Unix socket paths are length-limited; t.TempDir() can exceed that.
	runtimeDir, err := os.MkdirTemp("/tmp", "aishwnd-auth-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(runtimeDir) })
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	inRead, inWrite := io.Pipe()
	outRead, outWrite := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), inRead, outWrite) }()
	t.Cleanup(func() {
		inWrite.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("Run did not return after its stdin closed")
		}
		outRead.Close()
	})

	peer := aishwinwire.NewConn(outRead, inWrite)
	hello, err := json.Marshal(aishwinwire.HelloData{Proto: aishwinwire.ProtoVersion, Name: "auth-test", AvailableShells: []string{"cmd"}, DefaultShell: "cmd"})
	if err != nil {
		t.Fatal(err)
	}
	if err := peer.Send(aishwinwire.Frame{Type: "hello", Data: hello}); err != nil {
		t.Fatal(err)
	}
	ackFrame, err := peer.ReadOne()
	if err != nil || ackFrame.Type != "hello_ack" {
		t.Fatalf("expected hello_ack, got %+v (err %v)", ackFrame, err)
	}
	var ack aishwinwire.HelloAckData
	if err := json.Unmarshal(ackFrame.Data, &ack); err != nil {
		t.Fatal(err)
	}

	listAnswered := make(chan bool, 1)
	go peer.ReadLoop(func(f aishwinwire.Frame) {
		if f.Type != "prompt" {
			return
		}
		go func() {
			ch := peer.Await("poll")
			_ = peer.Send(aishwinwire.Frame{Type: "list_clients", ID: "poll"})
			select {
			case <-ch:
				listAnswered <- true
			case <-time.After(3 * time.Second):
				peer.CancelAwait("poll")
				listAnswered <- false
			}
			answer, _ := json.Marshal(aishwinwire.PromptAnswerData{Answer: "y"})
			_ = peer.Send(aishwinwire.Frame{Type: "prompt_answer", ID: f.ID, Data: answer})
		}()
	})

	conn, err := net.Dial("unix", paths.Socket(ack.SessionID))
	if err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "auth-test-client", Version: "test"}, nil)
	cs, err := client.Connect(context.Background(), &mcp.IOTransport{Reader: conn, Writer: conn}, nil)
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cs.Close()
		conn.Close()
	})

	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      authproto.RequestAccessTool,
		Arguments: authproto.RequestAccessArgs{PublicKey: base64.RawURLEncoding.EncodeToString(public)},
	})
	if err != nil {
		t.Fatalf("request_access did not complete: %v", err)
	}
	if res.IsError {
		t.Fatalf("request_access errored: %#v", res.Content)
	}
	if !<-listAnswered {
		t.Fatal("list_clients went unanswered while the approval prompt was pending")
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var out authproto.RequestAccessResult
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.GrantID == "" {
		t.Fatal("approved request_access returned no grant ID")
	}
}
