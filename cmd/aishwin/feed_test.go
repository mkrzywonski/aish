package main

import "testing"

func resetFeed() {
	feedMu.Lock()
	defer feedMu.Unlock()
	feedEntries = nil
	feedNextSeq = 1
	feedDropped = 0
}

func TestFeedReadsIncrementally(t *testing.T) {
	resetFeed()
	feedAppend("Running dir", "command")
	feedAppend("Reading C:\\a.txt", "file")

	got, next, dropped := feedSince(0, 10)
	if len(got) != 2 || dropped != 0 {
		t.Fatalf("first read got %d entries, dropped=%d; want 2 and 0", len(got), dropped)
	}
	if got[0].Text != "Running dir" || got[1].Kind != "file" {
		t.Errorf("unexpected entries: %+v", got)
	}

	// Nothing new: the cursor must not go backwards and must return nothing.
	again, next2, _ := feedSince(next, 10)
	if len(again) != 0 {
		t.Errorf("re-reading at the cursor returned %d entries, want 0", len(again))
	}
	if next2 < next {
		t.Errorf("cursor went backwards: %d then %d", next, next2)
	}

	feedAppend("Writing C:\\b.txt", "file")
	fresh, _, _ := feedSince(next2, 10)
	if len(fresh) != 1 || fresh[0].Text != "Writing C:\\b.txt" {
		t.Errorf("incremental read got %+v, want just the new line", fresh)
	}
}

func TestFeedRespectsMax(t *testing.T) {
	resetFeed()
	for i := 0; i < 10; i++ {
		feedAppend("line", "command")
	}
	got, next, _ := feedSince(0, 3)
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3", len(got))
	}
	rest, _, _ := feedSince(next, 100)
	if len(rest) != 7 {
		t.Errorf("second page got %d entries, want 7", len(rest))
	}
}

// A reader that falls behind must be told it missed something rather than
// silently seeing a gap.
func TestFeedReportsEvictions(t *testing.T) {
	resetFeed()
	for i := 0; i < feedCapacity+25; i++ {
		feedAppend("line", "command")
	}
	if _, _, dropped := feedSince(1, 10); dropped != 25 {
		t.Errorf("dropped = %d, want 25", dropped)
	}
	// A first-time reader has not missed anything it asked for.
	if _, _, dropped := feedSince(0, 10); dropped != 0 {
		t.Errorf("a fresh reader was told it dropped %d", dropped)
	}
}

func TestFeedBoundsMemory(t *testing.T) {
	resetFeed()
	for i := 0; i < feedCapacity*3; i++ {
		feedAppend("line", "command")
	}
	feedMu.Lock()
	n := len(feedEntries)
	feedMu.Unlock()
	if n > feedCapacity {
		t.Errorf("retained %d entries, cap is %d", n, feedCapacity)
	}
}
