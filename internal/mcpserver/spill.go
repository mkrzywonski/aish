package mcpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"ai-ssh/internal/outcap"
	"ai-ssh/internal/sshmux"
)

// exec has no scrollback behind it.
//
// run_command types into the shared terminal, so its output lands in the ring
// and a truncated result stays retrievable: it reports cursor_start/cursor_end
// and its truncation notice says to fetch the rest with read_output. exec is
// out-of-band — nothing touches the terminal, so once its output was trimmed
// the omitted bytes were gone, the notice offered no way to get them, and no
// field even said truncation had happened.
//
// The spill file is the ring's analogue for a path with no buffer: the full
// output is written on the host that ran the command, and the result names it.
// Retrieval then needs no new machinery — file_read pages it, file_grep
// searches it without reading it.
//
// One file exists at a time per session. The previous one is removed when a
// new one is written, which keeps cleanup to a step aish already performs on a
// route it already holds open. A background collector would have to reach
// hosts at teardown, exactly when the connection may be gone, and on an
// MFA-protected host could cost a 2FA push to delete a temp file.

// execOutputInline bounds what a single exec returns inline, shared with every
// other path so output is not spilled at one threshold and trimmed at another.
const execOutputInline = outcap.MaxInline

type spillState struct {
	mu   sync.Mutex
	path string
	ci   *sshmux.ConnInfo // nil when the previous spill was written locally
}

// spillOutput writes full output for later retrieval and returns its path.
// A failure is reported to the caller rather than swallowed: claiming the
// trimmed text is everything would be worse than saying the rest was lost.
func (c *Core) spillOutput(rt route, sessionID string, data []byte) (string, error) {
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", err
	}
	name := "aish-output-" + sessionID + "-" + hex.EncodeToString(suffix[:]) + ".txt"

	c.spill.mu.Lock()
	prev, prevCI := c.spill.path, c.spill.ci
	c.spill.mu.Unlock()

	switch rt.via {
	case "local":
		path := filepath.Join(os.TempDir(), name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return "", err
		}
		c.removeSpill(prev, prevCI)
		c.spill.mu.Lock()
		c.spill.path, c.spill.ci = path, nil
		c.spill.mu.Unlock()
		return path, nil
	case "controlmaster", "channel":
		caps, _ := c.Mux.CachedCapabilities(rt.ci)
		path := "/tmp/" + name
		script := sshmux.WriteScript(path, data, false, "0600", caps.Base64Decode())
		res, err := c.Mux.ChannelRun(rt.ci, script, 60*time.Second)
		if err != nil {
			return "", err
		}
		if res.Exit != 0 {
			return "", fmt.Errorf("writing %s: %s", path, trimOutput(string(res.Output)))
		}
		c.removeSpill(prev, prevCI)
		c.spill.mu.Lock()
		c.spill.path, c.spill.ci = path, rt.ci
		c.spill.mu.Unlock()
		return path, nil
	}
	return "", fmt.Errorf("no route able to store the full output")
}

// removeSpill deletes the previous spill, on whichever host holds it. Best
// effort: a host we can no longer reach keeps one stale temp file, which is a
// better outcome than failing the command that succeeded.
func (c *Core) removeSpill(path string, ci *sshmux.ConnInfo) {
	if path == "" {
		return
	}
	if ci == nil {
		_ = os.Remove(path)
		return
	}
	_, _ = c.Mux.ChannelRun(ci, "rm -f -- "+sshmux.Quote(path), 20*time.Second)
}

// capExecOutput trims from the middle, keeping both ends, and reports whether
// it had to. The conclusion of a command is usually its last line, so the end
// is never what gets cut.
func capExecOutput(b []byte) (string, bool) {
	if len(b) <= execOutputInline {
		return string(b), false
	}
	half := execOutputInline / 2
	head, tail := limitHeadBytes(b, half), limitTailBytes(b, half)
	return string(head) +
		fmt.Sprintf("\n... [%d bytes omitted from the middle; the full output is in the file named by output_path] ...\n",
			len(b)-len(head)-len(tail)) +
		string(tail), true
}

// limitHeadBytes and limitTailBytes back off to a rune boundary so a cut can
// never produce invalid UTF-8.
func limitHeadBytes(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	for n > 0 && b[n]&0xC0 == 0x80 {
		n--
	}
	return b[:n]
}

func limitTailBytes(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	start := len(b) - n
	for start < len(b) && b[start]&0xC0 == 0x80 {
		start++
	}
	return b[start:]
}

// attachSpill trims oversized output and records where the full text went.
func (c *Core) attachSpill(ctx context.Context, res *execResult, rt route, sessionID string, full []byte) {
	out, truncated := capExecOutput(full)
	res.Output = out
	if !truncated {
		return
	}
	res.Truncated = true
	res.OutputBytes = int64(len(full))
	path, err := c.spillOutput(rt, sessionID, full)
	if err != nil {
		res.Warning = "output was trimmed and the full text could not be saved: " + err.Error()
		return
	}
	res.OutputPath = path
}
