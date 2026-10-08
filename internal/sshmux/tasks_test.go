package sshmux

import (
	"os/exec"
	"testing"
	"time"
)

func TestLocalTaskCompletes(t *testing.T) {
	table := NewTable()
	task, err := table.Start(exec.Command("sh", "-c", "sleep 0.1; printf done"))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
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

// TestTaskCapturesASingleStream guards the merge. os/exec reuses one pipe and
// one copying goroutine only while Stdout and Stderr compare equal; give them
// separate writers and the two streams are spliced together in whatever order
// the copying goroutines happen to run, not the order the remote wrote them.
func TestTaskCapturesASingleStream(t *testing.T) {
	cmd := exec.Command("sh", "-c", "true")
	if _, err := NewTable().Start(cmd); err != nil {
		t.Fatal(err)
	}
	if cmd.Stdout != cmd.Stderr {
		t.Errorf("Stdout (%T) and Stderr (%T) differ; os/exec will open two pipes and reorder the merged output", cmd.Stdout, cmd.Stderr)
	}
}

func TestChannelTaskLimitAndRemove(t *testing.T) {
	table := NewTable()
	ci := &ConnInfo{Host: "h", User: "u", Sock: "/s"}
	var ids []string
	for i := 0; i < MaxChannelTasks; i++ {
		task, err := table.NewChannelTask(ci, "sess")
		if err != nil {
			t.Fatalf("task %d: %v", i, err)
		}
		ids = append(ids, task.ID)
	}
	if _, err := table.NewChannelTask(ci, "sess"); err == nil {
		t.Fatal("expected the per-session channel task limit to refuse")
	}
	table.Remove(ids[0])
	if _, err := table.NewChannelTask(ci, "sess"); err != nil {
		t.Fatalf("a removed task should free its slot: %v", err)
	}
	if _, err := NewTable().NewChannelTask(ci, "bad/id"); err == nil {
		t.Fatal("expected an invalid session id to be refused")
	}
}
