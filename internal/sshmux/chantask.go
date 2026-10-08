// Channel tasks: background commands launched and observed through the
// persistent OOB channel instead of a dedicated ControlMaster slave session.
//
// A dedicated slave per background task cost one MFA push per task on hosts
// with per-session Duo. Here the launch and every poll are ordinary scripts
// on the already-open `sh -s`, so a warm channel opens nothing new.
//
// The launcher starts a detached supervisor (setsid when present, else
// nohup; HUP ignored either way) with every fd redirected, so neither the
// supervisor nor the command can ever write into the channel's framing or
// keep the channel's ssh session open. All state lives in one per-task
// directory on the remote:
//
//	cmd      the command text, run as `sh cmd` so `exit`/`exec` in it cannot
//	         skip the supervisor's completion record
//	limit    output cap in bytes; run: the supervisor script (cwd is
//	         passed to it as an argument)
//	started  supervisor pid, published atomically before the command runs
//	spawnfail  the detacher's exit status when it failed before started
//	out      the first limit+1 bytes of combined output (the extra byte only
//	         flags that output was limited; polls expose at most limit)
//	rc       the command's exit status; headrc/drainrc: capture statuses
//	cdfail   present when cwd could not be entered
//	exit     "rc,headrc,drainrc", published atomically only after the
//	         output pipe has fully closed, so completion implies final output
//
// Nothing here ever opens a channel the caller did not already pay for:
// ChannelRun's own open path (with BeginSessionAttempt and the [m] block)
// is the only way a session starts.
package sshmux

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	// ChannelTaskOutputLimit caps the output retained per task on the remote.
	ChannelTaskOutputLimit = 16 << 20
	// MaxChannelTasks bounds how many channel tasks one session may create,
	// so retained output is bounded per session, not just per task.
	MaxChannelTasks = 16

	channelTaskLaunchTimeout = 30 * time.Second
	channelTaskPollTimeout   = 60 * time.Second
)

// Channel task states reported to callers. Only Done and CaptureFailed are
// terminal with a known command exit status; none of the others means the
// command is safe to run again.
const (
	TaskUncertain     = "uncertain"      // launch outcome unknown; may have started
	TaskStarting      = "starting"       // directory exists, supervisor not yet confirmed
	TaskRunning       = "running"        // supervisor alive, command running
	TaskDraining      = "draining"       // command exited; a descendant still holds its output open
	TaskDone          = "done"           // finished; exit_code is the command's status
	TaskCaptureFailed = "capture_failed" // finished, but output capture or status recording failed
	TaskLost          = "lost"           // supervisor gone without a completion record
	TaskExpired       = "expired"        // task directory no longer exists
	TaskStartFailed   = "start_failed"   // setsid/nohup failed; the command never ran
)

// ChannelTask is a background command on a remote, bound to the
// ControlMaster connection it was launched over.
type ChannelTask struct {
	CI  ConnInfo
	Dir string

	// submitted records that the launcher positively confirmed it created the
	// directory and submitted the supervisor. Without it a missing directory
	// proves nothing: a delayed launcher may still create it.
	submitted atomic.Bool
}

var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// newChannelTaskDir picks the task's remote directory before anything is
// sent, so its identity survives a lost launch acknowledgment.
func newChannelTaskDir(sessionID, taskID string) (string, error) {
	if !sessionIDPattern.MatchString(sessionID) || !sessionIDPattern.MatchString(taskID) {
		return "", fmt.Errorf("invalid task identity %q/%q", sessionID, taskID)
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "/tmp/aish-task-" + sessionID + "-" + taskID + "-" + hex.EncodeToString(b), nil
}

// supervisorScript is written to <dir>/run and executed by a fresh shell as
// `sh run <dir> <cwd>`. It must stay POSIX: it runs on whatever /bin/sh the
// remote has. It inherits the channel shell's directory, so a command with
// no cwd runs where a foreground exec would. cwd arrives as an argument, not
// via $(cat file), because command substitution strips trailing newlines
// and would silently change a path that ends in one.
const supervisorScript = `d=$1
cwd=$2
trap '' HUP
printf '%s\n' "$$" > "$d/started.tmp" && mv -f "$d/started.tmp" "$d/started" || exit 1
lim=$(cat "$d/limit")
{
  ok=1
  if [ -n "$cwd" ]; then cd "$cwd" || ok=0; fi
  if [ "$ok" = 1 ]; then
    sh "$d/cmd" </dev/null
    echo "$?" > "$d/rc"
  else
    : > "$d/cdfail"
    echo 1 > "$d/rc"
  fi
} 2>&1 | {
  head -c "$((lim + 1))" > "$d/out"
  echo "$?" > "$d/headrc"
  cat > /dev/null
  echo "$?" > "$d/drainrc"
}
r() { if [ -f "$1" ]; then cat "$1"; else echo -; fi; }
printf '%s,%s,%s\n' "$(r "$d/rc")" "$(r "$d/headrc")" "$(r "$d/drainrc")" > "$d/exit.tmp" &&
  mv -f "$d/exit.tmp" "$d/exit"
`

// channelTaskLaunchScript builds the launcher. It runs entirely inside a
// subshell so a failure can never exit the shared channel shell or leak its
// cwd/umask, and reports one AISHTASK line. The detacher runs inside a
// background watcher (fds redirected, HUP ignored, so it never holds the
// channel) that records spawnfail when setsid/nohup itself fails before the
// supervisor publishes started -- otherwise such a task would sit in
// "starting" forever. A zero exit is not treated as failure: some setsid
// implementations fork and return 0 at once.
func channelTaskLaunchScript(dir, command, cwd string, limit int) string {
	return fmt.Sprintf(`( umask 077
d=%s
mkdir -m 700 "$d" || { echo 'AISHTASK mkdir-failed'; exit 0; }
{ printf '%%s\n' %s > "$d/cmd" &&
  printf '%%s\n' %d > "$d/limit" &&
  printf '%%s' %s > "$d/run"; } || { rm -rf "$d"; echo 'AISHTASK setup-failed'; exit 0; }
( trap '' HUP
  if command -v setsid >/dev/null 2>&1; then
    setsid sh "$d/run" "$d" %s
  else
    nohup sh "$d/run" "$d" %s
  fi
  rc=$?
  [ "$rc" -ne 0 ] && [ ! -f "$d/started" ] && echo "$rc" > "$d/spawnfail"
) </dev/null >/dev/null 2>&1 &
echo 'AISHTASK submitted'
) </dev/null 2>&1`, Quote(dir), Quote(command), limit, Quote(supervisorScript), Quote(cwd), Quote(cwd))
}

// ErrTaskLaunchUncertain means the launch script was sent but its outcome
// is unknown: the command may be running. The task must be kept and
// reported, never silently retried.
var ErrTaskLaunchUncertain = errors.New("task launch outcome unknown")

// LaunchChannelTask starts command (in cwd, if set) as a detached task on
// t's remote. A nil error means the launcher confirmed submission. An error
// wrapping ErrTaskLaunchUncertain means the command may have started; any
// other error is a definite failure before anything was spawned.
func (m *Mux) LaunchChannelTask(t *ChannelTask, command, cwd string) error {
	// Open/probe first: a failure here happens before the launcher is sent,
	// so it is a definite failure, not an uncertain one.
	if _, err := m.EnsureProbed(&t.CI); err != nil {
		return err
	}
	res, err := m.ChannelRun(&t.CI, channelTaskLaunchScript(t.Dir, command, cwd, ChannelTaskOutputLimit), channelTaskLaunchTimeout)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrTaskLaunchUncertain, err)
	}
	if res.TimedOut {
		return fmt.Errorf("%w: the launcher timed out and the channel was closed", ErrTaskLaunchUncertain)
	}
	out := string(res.Output)
	switch {
	case strings.Contains(out, "AISHTASK submitted"):
		t.submitted.Store(true)
		return nil
	case strings.Contains(out, "AISHTASK mkdir-failed"):
		return fmt.Errorf("could not create task directory %s: %s", t.Dir, trimLaunchOutput(out))
	case strings.Contains(out, "AISHTASK setup-failed"):
		return fmt.Errorf("could not write task files in %s: %s", t.Dir, trimLaunchOutput(out))
	}
	return fmt.Errorf("%w: unrecognized launcher response %q", ErrTaskLaunchUncertain, trimLaunchOutput(out))
}

func trimLaunchOutput(s string) string {
	var keep []string
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if !strings.HasPrefix(line, "AISHTASK ") && strings.TrimSpace(line) != "" {
			keep = append(keep, strings.TrimSpace(line))
		}
	}
	msg := strings.Join(keep, "; ")
	if len(msg) > 300 {
		msg = msg[:300] + "..."
	}
	return msg
}

// ChannelTaskStatus is one poll of a channel task.
type ChannelTaskStatus struct {
	State         string
	Output        []byte
	Next          int64 // cursor for the next poll (start + len(Output))
	ExitCode      *int
	OutputLimited bool // output passed ChannelTaskOutputLimit; the rest was discarded
	CwdFailed     bool
	OutputMissing bool   // completed, but its output file no longer exists
	StartFailure  string // detacher exit status when State is TaskStartFailed
}

// channelTaskPollScript reports the task's state on one metadata line,
// followed by base64 of the selected output range. Completion is read FIRST
// and the output size after it, so a completed task's final bytes are never
// missed. Cursor rules mirror term.Ring.ReadFrom over the retained prefix:
// negative = the last n bytes, past-the-end clamps to the end.
func channelTaskPollScript(dir string, cursor int64, n, limit int) string {
	return fmt.Sprintf(`( d=%s; c=%d; n=%d; lim=%d
if [ ! -d "$d" ]; then echo 'AISHPOLL nodir'; exit 0; fi
fin=; [ -f "$d/exit" ] && fin=$(cat "$d/exit")
pid=; [ -f "$d/started" ] && pid=$(cat "$d/started")
alive=-
if [ -n "$pid" ] && [ -z "$fin" ]; then
  if kill -0 "$pid" 2>/dev/null; then alive=1; else alive=0; [ -f "$d/exit" ] && fin=$(cat "$d/exit"); fi
fi
rc=; [ -f "$d/rc" ] && rc=$(cat "$d/rc")
cdf=0; [ -f "$d/cdfail" ] && cdf=1
sf=; [ -z "$pid" ] && [ -f "$d/spawnfail" ] && sf=$(cat "$d/spawnfail")
of=0; size=0; [ -f "$d/out" ] && { of=1; size=$(wc -c < "$d/out" | tr -d ' '); }
vis=$size; [ "$vis" -gt "$lim" ] && vis=$lim
if [ "$c" -lt 0 ]; then s=$((vis - n)); [ "$s" -lt 0 ] && s=0; else s=$c; [ "$s" -gt "$vis" ] && s=$vis; fi
k=$((vis - s)); [ "$k" -gt "$n" ] && k=$n
printf 'AISHPOLL ok pid=%%s alive=%%s rc=%%s cdf=%%s sf=%%s out=%%s size=%%s start=%%s count=%%s exit=%%s\n' "${pid:--}" "$alive" "${rc:--}" "$cdf" "${sf:--}" "$of" "$size" "$s" "$k" "${fin:--}"
if [ "$k" -gt 0 ]; then tail -c +$((s + 1)) "$d/out" | head -c "$k" | base64; fi
) </dev/null 2>/dev/null`, Quote(dir), cursor, n, limit)
}

// PollChannelTask reads t's state and up to max bytes of output from cursor
// over the existing persistent channel. Transport failures are returned as
// errors (status unknown), never reported as a lost task.
func (m *Mux) PollChannelTask(t *ChannelTask, cursor int64, max int) (ChannelTaskStatus, error) {
	if cursor < 0 {
		cursor = -1
	}
	if cursor > ChannelTaskOutputLimit {
		cursor = ChannelTaskOutputLimit
	}
	res, err := m.ChannelRun(&t.CI, channelTaskPollScript(t.Dir, cursor, max, ChannelTaskOutputLimit), channelTaskPollTimeout)
	if err != nil {
		return ChannelTaskStatus{}, err
	}
	if res.TimedOut {
		return ChannelTaskStatus{}, errors.New("polling the task timed out and the shared channel was closed; the task itself is unaffected")
	}
	st, err := parseChannelTaskPoll(res.Output, t.submitted.Load())
	if err == nil && st.OutputMissing && cursor >= 0 {
		// Don't rewind the caller to 0 as though the output had never existed.
		st.Next = cursor
	}
	return st, err
}

func parseChannelTaskPoll(out []byte, submitted bool) (ChannelTaskStatus, error) {
	header, body, _ := bytes.Cut(out, []byte("\n"))
	line := strings.TrimRight(string(header), "\r")
	if line == "AISHPOLL nodir" {
		if submitted {
			return ChannelTaskStatus{State: TaskExpired}, nil
		}
		// The launcher may not have run yet: absence proves nothing.
		return ChannelTaskStatus{State: TaskUncertain}, nil
	}
	rest, ok := strings.CutPrefix(line, "AISHPOLL ok ")
	if !ok {
		return ChannelTaskStatus{}, fmt.Errorf("malformed task status %.120q", line)
	}
	kv := map[string]string{}
	for _, f := range strings.Fields(rest) {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			return ChannelTaskStatus{}, fmt.Errorf("malformed task status field %q", f)
		}
		kv[k] = v
	}
	num := func(k string) (int64, error) {
		v, err := strconv.ParseInt(kv[k], 10, 64)
		if err != nil || v < 0 {
			return 0, fmt.Errorf("malformed task status %s=%q", k, kv[k])
		}
		return v, nil
	}
	size, err := num("size")
	if err != nil {
		return ChannelTaskStatus{}, err
	}
	start, err := num("start")
	if err != nil {
		return ChannelTaskStatus{}, err
	}
	count, err := num("count")
	if err != nil {
		return ChannelTaskStatus{}, err
	}
	data, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(string(body)), ""))
	if err != nil {
		return ChannelTaskStatus{}, fmt.Errorf("malformed task output: %v", err)
	}
	if int64(len(data)) != count {
		return ChannelTaskStatus{}, fmt.Errorf("task output read returned %d bytes, expected %d", len(data), count)
	}

	st := ChannelTaskStatus{
		Output:        data,
		Next:          start + count,
		OutputLimited: size > ChannelTaskOutputLimit,
		CwdFailed:     kv["cdf"] == "1",
	}
	if fin := kv["exit"]; fin != "-" && fin != "" {
		parts := strings.Split(fin, ",")
		if len(parts) != 3 {
			return ChannelTaskStatus{}, fmt.Errorf("malformed task completion record %q", fin)
		}
		st.State = TaskDone
		if rc, err := strconv.Atoi(parts[0]); err == nil {
			st.ExitCode = &rc
		} else {
			st.State = TaskCaptureFailed
		}
		if parts[1] != "0" || parts[2] != "0" {
			st.State = TaskCaptureFailed
		}
		// The supervisor always creates out before publishing exit, so a
		// completed task without it lost its output afterwards (e.g. a /tmp
		// cleaner). Never present that as a successful empty capture.
		if kv["out"] != "1" {
			st.State = TaskCaptureFailed
			st.OutputMissing = true
		}
		return st, nil
	}
	// Liveness comes from kill -0 on the recorded pid, which pid reuse can
	// fool; callers present running/lost as observations, not proof.
	switch kv["alive"] {
	case "1":
		st.State = TaskRunning
		if kv["rc"] != "-" {
			st.State = TaskDraining
		}
	case "0":
		st.State = TaskLost
	default: // no started record yet
		st.State = TaskStarting
		if sf := kv["sf"]; sf != "-" && sf != "" {
			st.State = TaskStartFailed
			st.StartFailure = sf
		}
	}
	return st, nil
}
