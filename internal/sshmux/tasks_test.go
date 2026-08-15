package sshmux

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestMarkerWriterFiltersSplitMarker(t *testing.T) {
	var out bytes.Buffer
	ready := make(chan struct{})
	w := &markerWriter{dst: &out, marker: []byte("@READY@\n"), ready: func() { close(ready) }}
	for _, part := range []string{"before\n@RE", "ADY", "@\nafter\n"} {
		if _, err := w.Write([]byte(part)); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-ready:
	default:
		t.Fatal("marker did not signal readiness")
	}
	w.Close()
	if got := out.String(); got != "before\nafter\n" {
		t.Fatalf("filtered output = %q", got)
	}
}

func TestBackgroundCommandAndTaskReadiness(t *testing.T) {
	command, marker, err := BackgroundCommand("sleep 0.2; printf done")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(command, "sleep 0.2") || !strings.HasPrefix(string(marker), "@AISHSTART@") {
		t.Fatalf("wrapped command = %q marker=%q", command, marker)
	}

	ready := make(chan struct{})
	table := NewTable()
	task, err := table.StartAfterMarker(exec.Command("sh", "-c", command), marker, func() { close(ready) })
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("startup marker was not observed")
	}
	if running, _ := task.Status(); !running {
		t.Fatal("task completed before readiness was signaled")
	}

	deadline := time.Now().Add(time.Second)
	for {
		if running, _ := task.Status(); !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("task did not complete")
		}
		time.Sleep(10 * time.Millisecond)
	}
	out, _, _ := task.Out.ReadFrom(0, 1024)
	if got := string(out); got != "done" {
		t.Fatalf("task output = %q", got)
	}
}

func TestTaskWithoutMarkerStillClearsReadiness(t *testing.T) {
	ready := make(chan struct{})
	table := NewTable()
	_, err := table.StartAfterMarker(exec.Command("sh", "-c", "printf failure >&2"), []byte("missing"), func() { close(ready) })
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("process exit did not clear readiness")
	}
}

// TestTaskCapturesASingleStream guards the merge. os/exec reuses one pipe and
// one copying goroutine only while Stdout and Stderr compare equal; give them
// separate writers and the two streams are spliced together in whatever order
// the copying goroutines happen to run, not the order the remote wrote them.
func TestTaskCapturesASingleStream(t *testing.T) {
	cases := []struct {
		name   string
		marker []byte
	}{
		{"plain", nil},
		{"with startup marker", []byte("@READY@\n")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			table := NewTable()
			cmd := exec.Command("sh", "-c", "true")
			var err error
			if tc.marker == nil {
				_, err = table.Start(cmd)
			} else {
				_, err = table.StartAfterMarker(cmd, tc.marker, func() {})
			}
			if err != nil {
				t.Fatal(err)
			}
			if cmd.Stdout != cmd.Stderr {
				t.Errorf("Stdout (%T) and Stderr (%T) differ; os/exec will open two pipes and reorder the merged output", cmd.Stdout, cmd.Stderr)
			}
		})
	}
}
