//go:build windows

package main

import (
	"encoding/json"
	"time"

	"ai-ssh/internal/aishwinwire"
)

// handleConsoleRead serves the retained console feed. It deliberately does not
// log itself: reading the feed is not an operation on this machine, and an
// entry saying "someone read the feed" would grow the feed every time anyone
// looked at it.
func handleConsoleRead(wc *aishwinwire.Conn, f aishwinwire.Frame) {
	var req aishwinwire.ConsoleReadData
	if err := json.Unmarshal(f.Data, &req); err != nil {
		send(wc, "console_read_result", f.ID, aishwinwire.ConsoleReadResultData{Error: err.Error()})
		return
	}
	entries, next, dropped := feedSince(req.Cursor, req.Max)
	out := aishwinwire.ConsoleReadResultData{NextCursor: next, Dropped: dropped}
	for _, e := range entries {
		out.Entries = append(out.Entries, aishwinwire.ConsoleEntry{
			Seq:  e.Seq,
			At:   e.At.Format(time.RFC3339),
			Text: e.Text,
			Kind: e.Kind,
		})
	}
	send(wc, "console_read_result", f.ID, out)
}
