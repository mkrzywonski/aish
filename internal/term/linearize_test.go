package term

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/quick"
)

// conptyCapture is a REAL byte sequence pulled from a live aish session: a
// cmd.exe shell reached over ssh from WSL, running
//
//	echo AISH-START & ver & echo AISH-END
//
// ConPTY ended each logical line with an absolute cursor move rather than a
// newline. This is the regression case the whole change exists for.
const conptyCapture = "AISH-START \x1b[7;1HMicrosoft Windows [Version 10.0.22631.3155]\r\n" +
	"AISH-END\x1b[10;1Htxstate\\mk31@TAG232207 C:\\Users\\mk31>\x1b[?25h"

func TestLinearizeConptyCapture(t *testing.T) {
	want := "AISH-START \nMicrosoft Windows [Version 10.0.22631.3155]\r\n" +
		"AISH-END\n\ntxstate\\mk31@TAG232207 C:\\Users\\mk31>"

	got := string(Linearize([]byte(conptyCapture), 24))
	if got != want {
		t.Errorf("Linearize(conptyCapture):\n got %q\nwant %q", got, want)
	}
}

// TestStripEscapesFusesConptyCapture pins the CURRENT behaviour, so the test
// file documents exactly what changed: StripEscapes deletes the cursor moves
// and fuses two logical lines into one.
func TestStripEscapesFusesConptyCapture(t *testing.T) {
	got := string(StripEscapes([]byte(conptyCapture)))
	if !strings.Contains(got, "AISH-START Microsoft Windows") {
		t.Errorf("expected StripEscapes to fuse the lines, got %q", got)
	}
}

// TestConptyCaptureSurvivesPromptTrim is the assertion that would have caught
// this bug in production. framing.runIdle drops everything after the final
// newline, on the assumption that the trailing unterminated line is the shell
// prompt. With the cursor moves deleted, "AISH-END" looked like part of the
// prompt line and vanished with it; linearized, it survives and only the prompt
// is dropped.
func TestConptyCaptureSurvivesPromptTrim(t *testing.T) {
	trim := func(s string) string {
		if i := strings.LastIndexByte(s, '\n'); i >= 0 {
			return s[:i+1]
		}
		return ""
	}

	if out := trim(string(StripEscapes([]byte(conptyCapture)))); strings.Contains(out, "AISH-END") {
		t.Fatalf("precondition failed: the bug is supposed to lose AISH-END, got %q", out)
	}

	out := trim(string(Linearize([]byte(conptyCapture), 24)))
	if !strings.Contains(out, "AISH-END") {
		t.Errorf("AISH-END was dropped by the prompt trim: %q", out)
	}
	if strings.Contains(out, "TAG232207") {
		t.Errorf("the prompt line should have been trimmed away: %q", out)
	}
}

func TestLinearize(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain text", "plain text\nmore\n", "plain text\nmore\n"},
		{"sgr only", "\x1b[0;32mgreen\x1b[0m\n", "green\n"},
		{"bare CR progress bar", "10%\r20%\r30%\n", "10%\r20%\r30%\n"},
		{"erase line inserts nothing", "line\x1b[Kmore\n", "linemore\n"},
		{"horizontal moves insert nothing", "a\x1b[5Cb\x1b[3Dc", "abc"},
		{"IND and NEL descend, RI is silent", "a\x1bDb\x1bEc\x1bMd", "a\nb\ncd"},
		{"same-row reposition is a CR, not a newline", "x\x1b[3;1Ha\x1b[3;1Hb", "x\na\rb"},
		{"upward move resyncs the row", "\x1b[10;1Ha\x1b[3;1Hb\x1b[5;1Hc", "ab\n\nc"},
		{"unknown row, column not 1, inserts nothing", "abc\x1b[9;40Hdef", "abcdef"},
		{"relative CUD needs no absolute state", "a\x1b[3Bb", "a\n\n\nb"},
		{"VPA moves the row, leaves the column", "\x1b[2;1Ha\x1b[4db", "a\n\nb"},
		{"BEL is dropped", "a\x07b", "ab"},
		{
			"DECSC/DECRC restore the row so the next move is not bogus",
			"\x1b[5;1Ha\x1b7\x1b[8;1Hb\x1b8\x1b[6;1Hc",
			"a\n\n\nb\nc",
		},
		{
			"alt screen desyncs the row rather than computing across it",
			"\x1b[3;1Ha\x1b[?1049hZ\x1b[?1049lb\x1b[9;1Hc",
			"aZb\nc",
		},
		{
			"charset designation and DECSC/DECRC are stripped",
			"\x1b(Ba\x1b7b\x1b8c",
			"abc",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(Linearize([]byte(tc.in), 24)); got != tc.want {
				t.Errorf("Linearize(%q):\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestLinearizeGrowthBudget(t *testing.T) {
	// Pathological alternation: every pair of moves wants a full screen of
	// newlines. The budget must hold the output under 2x input.
	in := []byte(strings.Repeat("a\x1b[1;1H\x1b[200;1H", 4000))
	out := Linearize(in, 24)
	if len(out) >= 2*len(in) {
		t.Errorf("growth budget exceeded: in %d bytes, out %d bytes", len(in), len(out))
	}
}

func TestLinearizeMidStreamNeverFuses(t *testing.T) {
	// P4: a ring read can start at any offset. For every possible starting
	// point, the two logical lines must never come back fused.
	b := []byte(conptyCapture)
	for k := range b {
		out := string(Linearize(b[k:], 24))
		if strings.Contains(out, "AISH-ENDtxstate") {
			t.Fatalf("fused at offset %d: %q", k, out)
		}
	}
}

// --- Properties -----------------------------------------------------------

// dropBreaks removes the bytes Linearize is allowed to insert.
//
// Deliberately byte-wise rather than bytes.Map: Map decodes UTF-8, so on the
// arbitrary binary that quick.Check generates every invalid byte becomes
// U+FFFD, and inserting a newline shifts how the following bytes decode. That
// made this helper report mismatches that had nothing to do with Linearize —
// and, worse, made the property flaky, since quick.Check seeds randomly and
// only sometimes produced input that tripped it.
func dropBreaks(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c != '\n' && c != '\r' {
			out = append(out, c)
		}
	}
	return out
}

// TestPropContentPreserved is P1, the ceiling on how wrong a mistake can be:
// linearization may only ADD line structure, never drop content.
func TestPropContentPreserved(t *testing.T) {
	f := func(b []byte) bool {
		return bytes.Equal(dropBreaks(Linearize(b, 24)), dropBreaks(StripEscapes(b)))
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 3000}); err != nil {
		t.Error(err)
	}
	// Random bytes rarely form escape sequences, so also drive it with input
	// built from a terminal-ish alphabet.
	for _, s := range escapeSoupCorpus() {
		if !f([]byte(s)) {
			t.Errorf("content not preserved for %q", s)
		}
	}
}

// TestPropNoOpWithoutVerticalMovement is P2, the regression bound for ordinary
// Linux sessions: with no vertical-movement construct in the stream, Linearize
// is byte-identical to StripEscapes.
func TestPropNoOpWithoutVerticalMovement(t *testing.T) {
	check := func(b []byte) bool {
		if hasVerticalMove(b) {
			return true
		}
		return bytes.Equal(Linearize(b, 24), StripEscapes(b))
	}
	if err := quick.Check(check, &quick.Config{MaxCount: 3000}); err != nil {
		t.Error(err)
	}
	for _, s := range escapeSoupCorpus() {
		if !check([]byte(s)) {
			t.Errorf("not a no-op for %q:\n got %q\nwant %q",
				s, Linearize([]byte(s), 24), StripEscapes([]byte(s)))
		}
	}
	// The realistic Linux surfaces, spelled out.
	for _, s := range []string{
		"\x1b[1;32muser@host\x1b[0m:\x1b[1;34m~/src\x1b[0m$ ",
		"Downloading  \x1b[K45%\r\x1b[K90%\r\x1b[K100%\n",
		"\x1b]7;file://host/home/mike\x1b\\\x1b]133;A\x1b\\$ ",
		"\x1b[?25lspinner\x1b[?25h\n",
		"\x1b[38;5;214mwarn\x1b[0m: something\n",
	} {
		if got, want := Linearize([]byte(s), 24), StripEscapes([]byte(s)); !bytes.Equal(got, want) {
			t.Errorf("Linux stream changed:\n  in  %q\n  got %q\n want %q", s, got, want)
		}
	}
}

func TestPropBoundedGrowth(t *testing.T) {
	f := func(b []byte) bool { return len(Linearize(b, 24)) < 2*len(b)+16 }
	if err := quick.Check(f, &quick.Config{MaxCount: 3000}); err != nil {
		t.Error(err)
	}
}

// hasVerticalMove reports whether b contains any construct that Linearize may
// translate into a break. Only these can make it diverge from StripEscapes.
func hasVerticalMove(b []byte) bool {
	for _, m := range ansiRe.FindAllIndex(b, -1) {
		seq := b[m[0]:m[1]]
		if len(seq) < 2 {
			continue
		}
		if seq[1] == '[' {
			if len(seq) < 3 { // truncated CSI, no parameters, no effect
				continue
			}
			body := seq[2 : len(seq)-1]
			if len(body) > 0 && (body[0] == '?' || body[0] == '<' || body[0] == '=' || body[0] == '>') {
				continue
			}
			// H/f (CUP), d (VPA), B/e (CUD/VPR), E (CNL) all emit; note the
			// case distinction from D (CUB) and A/F (upward), which do not.
			if bytes.IndexByte([]byte("HfdBeE"), seq[len(seq)-1]) >= 0 {
				return true
			}
			continue
		}
		if len(seq) == 2 && (seq[1] == 'D' || seq[1] == 'E') { // IND, NEL
			return true
		}
	}
	return false
}

// escapeSoupCorpus builds inputs dense in real escape sequences, since random
// bytes almost never produce one.
func escapeSoupCorpus() []string {
	frags := []string{
		"a", "bc", " ", "\n", "\r\n", "\r", "\x07", "\t",
		"\x1b[H", "\x1b[2;5H", "\x1b[7;1H", "\x1b[10;1H", "\x1b[1;1H",
		"\x1b[3A", "\x1b[2B", "\x1b[4C", "\x1b[5D", "\x1b[6d", "\x1b[2E", "\x1b[3F",
		"\x1b[K", "\x1b[2J", "\x1b[0m", "\x1b[31m", "\x1b[?25l", "\x1b[?25h",
		"\x1b[?1049h", "\x1b[?1049l", "\x1b[1;24r", "\x1b[s", "\x1b[u",
		"\x1bD", "\x1bE", "\x1bM", "\x1b7", "\x1b8", "\x1bc", "\x1b(B", "\x1b=",
		"\x1b]7;file://h/p\x1b\\", "\x1b]0;title\x07",
	}
	var out []string
	// Deterministic pseudo-random walk over the fragments.
	seed := 1
	for n := 0; n < 400; n++ {
		var sb strings.Builder
		for i := 0; i < 12; i++ {
			seed = (seed*1103515245 + 12345) & 0x7fffffff
			sb.WriteString(frags[seed%len(frags)])
		}
		out = append(out, sb.String())
	}
	return out
}

// --- Real capture corpus --------------------------------------------------

// TestCorpusRealCaptures runs the properties over byte streams recorded from
// real programs on a real PTY (see testdata/README.md). The synthetic corpus
// above is me guessing what terminals emit; this is what they actually emit,
// and it is the difference between reasoning about the blast radius on
// ordinary Linux sessions and measuring it.
func TestCorpusRealCaptures(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "*.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("capture corpus is missing; see testdata/README.md to regenerate")
	}

	for _, path := range files {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Run(filepath.Base(path), func(t *testing.T) {
			lin := Linearize(b, 24)
			strip := StripEscapes(b)

			// P1: linearization may only add line structure, never lose text.
			if !bytes.Equal(dropBreaks(lin), dropBreaks(strip)) {
				t.Error("content changed beyond added line breaks")
			}
			// P3: bounded growth on real input, not just adversarial input.
			if len(lin) >= 2*len(b)+16 {
				t.Errorf("growth unbounded: %d bytes in, %d out", len(b), len(lin))
			}
			// P2: the load-bearing one. A stream with no vertical-movement
			// construct must come back byte-identical, which is what makes
			// "ordinary Linux sessions are unaffected" a measurement.
			if !hasVerticalMove(b) {
				if !bytes.Equal(lin, strip) {
					t.Error("a stream containing no vertical movement was modified")
				}
				return
			}
			t.Logf("contains vertical movement: %d bytes in, %d stripped, %d linearized",
				len(b), len(strip), len(lin))
		})
	}
}

// TestCorpusCoversBothCases guards the corpus itself. If the captures with
// vertical movement were ever dropped, TestCorpusRealCaptures would still pass
// while silently testing nothing interesting.
func TestCorpusCoversBothCases(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join("testdata", "*.bin"))
	var withMove, withoutMove int
	for _, path := range files {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if hasVerticalMove(b) {
			withMove++
		} else {
			withoutMove++
		}
	}
	if withoutMove == 0 {
		t.Error("corpus has no capture free of vertical movement — the no-op property is untested on real data")
	}
	if withMove == 0 {
		t.Error("corpus has no capture containing vertical movement — linearization is untested on real data")
	}
}

// TestLinearizeDisabledFallsBackToStripping covers the AISH_NO_LINEARIZE
// escape hatch. It exists to rescue a live session without a rebuild, so it
// has to work at the moment it is reached for.
func TestLinearizeDisabledFallsBackToStripping(t *testing.T) {
	linearizeDisabled = true
	defer func() { linearizeDisabled = false }()

	got := Linearize([]byte(conptyCapture), 24)
	want := StripEscapes([]byte(conptyCapture))
	if !bytes.Equal(got, want) {
		t.Errorf("with linearization disabled:\n got %q\nwant %q", got, want)
	}
	// And the bug is back, which is the proof the switch really disabled it.
	if !strings.Contains(string(got), "AISH-START Microsoft Windows") {
		t.Error("expected the disabled path to reproduce plain stripping, fused lines and all")
	}
}

// --- FirstBreak -----------------------------------------------------------

func TestFirstBreak(t *testing.T) {
	// Expectations are written as the remainder after the break — that is what
	// callers actually slice with, and it doesn't invite byte-miscounting.
	cases := []struct {
		name string
		in   string
		rest string
		ok   bool
	}{
		{"newline terminated echo", "echo hi\r\nout", "out", true},
		{"cursor move terminated echo", "echo hi\x1b[7;1Hout", "out", true},
		{"no break at all", "echo hi\x1b[32m", "", false},
		{"leading move breaks nothing", "\x1b[7;1Hout", "", false},
		{"relative descent is a break", "echo hi\x1b[2Bout", "out", true},
		{"colour changes are not breaks", "\x1b[31mecho\x1b[0m hi\nx", "x", true},
		{"horizontal move is not a break", "echo hi\x1b[5Cout", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idx, ok := FirstBreak([]byte(tc.in))
			if ok != tc.ok {
				t.Fatalf("FirstBreak(%q) ok = %v, want %v", tc.in, ok, tc.ok)
			}
			if !ok {
				return
			}
			if rest := tc.in[idx:]; rest != tc.rest {
				t.Errorf("FirstBreak(%q) left %q, want %q", tc.in, rest, tc.rest)
			}
		})
	}
}

func TestFirstBreakOnCapture(t *testing.T) {
	// afterEcho's real job: find where the echoed command line ends. In the
	// captured stream that boundary is a CUP, not a newline — a newline-only
	// scan would skip past the version banner and eat it.
	idx, ok := FirstBreak([]byte(conptyCapture))
	if !ok {
		t.Fatal("expected a break in the capture")
	}
	rest := conptyCapture[idx:]
	if !strings.HasPrefix(rest, "Microsoft Windows") {
		t.Errorf("break landed in the wrong place; rest = %q", rest)
	}
}

// --- ansiRe extension -----------------------------------------------------

func TestStripEscapesCoversEcma48(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"DECSC/DECRC", "\x1b7a\x1b8b", "ab"},
		{"charset designation", "\x1b(Bx", "x"},
		{"keypad modes", "\x1b=y\x1b>z", "yz"},
		{"full reset", "\x1bca", "a"},
		{"CSI still wins", "\x1b[31mred\x1b[0m", "red"},
		{"OSC still wins", "\x1b]7;file://h/p\x1b\\x", "x"},
		{"OSC with BEL still wins", "\x1b]0;title\x07x", "x"},
		{"DCS still wins", "\x1bPsomething\x1b\\x", "x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(StripEscapes([]byte(tc.in))); got != tc.want {
				t.Errorf("StripEscapes(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
