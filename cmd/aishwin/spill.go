package main

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
)

// When a command's output is too large to return inline, the full text is
// written here and the result reports the path. Retrieval then costs no new
// machinery: file_read pages it and file_grep searches it without reading it,
// which is what you actually want when the useful five rows are buried in
// ninety stack traces.
//
// At most one spill file exists per session, because the previous one is
// deleted at the start of every command. That keeps cleanup to a single
// deterministic step on a path aish already controls, instead of a background
// collector — a collector would have to reach hosts at teardown, exactly when
// the connection may be gone, and on an MFA-protected host could cost the user
// a 2FA push to delete a temp file.
//
// The consequence a caller must know: retrieve the file BEFORE running another
// command, or it is gone. The tool description says so.
//
// Each spill carries a random suffix rather than a fixed per-session name. The
// invariant is the same, but a stale path then fails with "no such file"
// instead of quietly returning a different command's output — parallel tool
// calls and multiple AI clients on one session are both normal here, so the
// race is real and silent wrong data is the one outcome worth engineering out.

var (
	spillMu   sync.Mutex
	lastSpill string
)

// clearPreviousSpill removes the spill file from the previous command. Called
// at the start of every command, so a failure to write a new one still leaves
// no stale file claiming to be current.
func clearPreviousSpill() {
	spillMu.Lock()
	path := lastSpill
	lastSpill = ""
	spillMu.Unlock()
	if path != "" {
		_ = os.Remove(path)
	}
}

// writeSpill stores full output for later retrieval and returns its path.
func writeSpill(sessionID, data string) (string, error) {
	if sessionID == "" {
		sessionID = "session"
	}
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", err
	}
	path := filepath.Join(os.TempDir(), "aish-output-"+sessionID+"-"+hex.EncodeToString(suffix[:])+".txt")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		return "", err
	}
	spillMu.Lock()
	lastSpill = path
	spillMu.Unlock()
	return path, nil
}
