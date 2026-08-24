package main

import (
	"sync"
	"time"
)

// The console feed is what the human watching this machine actually sees: a
// scrolling account of every operation, rendered in aishwin's window. Until
// now it existed only there. An AI working on this session had no way to ask
// what had already happened, so an evaluation agent reconstructed the history
// by reading leftover files off disk, and learned that another client had been
// active only because a screenshot happened to catch the window mid-scroll.
//
// This is deliberately NOT an out-of-band log. On a shared-terminal session
// oob_log answers "what did you do that I could not see"; here there is no
// invisible route and nothing to recover. The feed is the visible channel
// itself, so its counterpart is the terminal's scrollback, and the tool that
// reads it carries the same name as the one that reads a terminal's.
//
// Kept in its own file, free of Win32, so the ring is exercised by tests on
// any platform rather than only where the GUI builds.

// feedCapacity bounds the retained feed. Old entries are evicted rather than
// growing without limit; a reader that misses them is told how many.
const feedCapacity = 500

// FeedEntry is one line as the human saw it.
type FeedEntry struct {
	Seq  int64     `json:"seq"`
	At   time.Time `json:"at"`
	Text string    `json:"text"`
	Kind string    `json:"kind,omitempty"` // "command", "file", "task", ... best effort
}

var (
	feedMu      sync.Mutex
	feedEntries []FeedEntry
	feedNextSeq int64 = 1
	feedDropped int64 // entries evicted before any reader asked for them
)

// feedAppend records one line. Called from the same place the GUI is told to
// draw it, so the retained feed and the visible window cannot disagree.
func feedAppend(text, kind string) {
	feedMu.Lock()
	defer feedMu.Unlock()
	feedEntries = append(feedEntries, FeedEntry{
		Seq:  feedNextSeq,
		At:   time.Now(),
		Text: text,
		Kind: kind,
	})
	feedNextSeq++
	if len(feedEntries) > feedCapacity {
		drop := len(feedEntries) - feedCapacity
		feedEntries = append([]FeedEntry(nil), feedEntries[drop:]...)
		feedDropped += int64(drop)
	}
}

// feedSince returns up to max entries with a sequence at or after cursor, the
// cursor to pass next time, and how many entries were evicted before being
// read. Cursor semantics mirror the shared-terminal side's read_output: next
// is the highest sequence EXAMINED, so a caller keeps advancing even across a
// window that returned nothing it wanted.
func feedSince(cursor int64, max int) (entries []FeedEntry, next int64, dropped int64) {
	feedMu.Lock()
	defer feedMu.Unlock()
	if max <= 0 || max > feedCapacity {
		max = feedCapacity
	}
	next = cursor
	for _, e := range feedEntries {
		if e.Seq < cursor {
			continue
		}
		if len(entries) == max {
			break
		}
		entries = append(entries, e)
		next = e.Seq + 1
	}
	if next < feedNextSeq && len(entries) == 0 {
		next = feedNextSeq
	}
	// Only report evictions to a reader that had already started reading;
	// a first call has not missed anything it asked for.
	if cursor > 0 && cursor <= feedDropped {
		dropped = feedDropped - cursor + 1
	}
	return entries, next, dropped
}
