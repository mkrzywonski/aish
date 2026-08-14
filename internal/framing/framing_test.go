package framing

import (
	"strings"
	"testing"
	"unicode/utf8"

	"ai-ssh/internal/term"
)

// conptyCapture is the real byte sequence captured from a live aish session —
// a cmd.exe shell reached over ssh from WSL, running
//
//	echo AISH-START & ver & echo AISH-END
//
// It is duplicated from internal/term's tests on purpose: this package needs to
// assert the COMPOSITION (linearize, then trim the prompt), which is where the
// output was actually being lost.
const conptyCapture = "AISH-START \x1b[7;1HMicrosoft Windows [Version 10.0.22631.3155]\r\n" +
	"AISH-END\x1b[10;1Htxstate\\mk31@TAG232207 C:\\Users\\mk31>\x1b[?25h"

// TestIdlePipelineKeepsFinalLine is the regression test for the whole defect:
// run_command on a ConPTY host returned output with its last line silently
// missing, because the prompt trim could not tell the final output line from
// the prompt once the cursor moves between them had been deleted.
func TestIdlePipelineKeepsFinalLine(t *testing.T) {
	// The old pipeline: strip escapes, then trim the trailing prompt.
	old := dropTrailingPrompt(string(term.StripEscapes([]byte(conptyCapture))), false)
	if strings.Contains(old, "AISH-END") {
		t.Fatalf("precondition failed: the bug is supposed to lose AISH-END, got %q", old)
	}

	// The new pipeline: linearize, then trim.
	got := dropTrailingPrompt(string(term.Linearize([]byte(conptyCapture), 24)), false)
	if !strings.Contains(got, "AISH-END") {
		t.Errorf("AISH-END lost by the idle pipeline: %q", got)
	}
	if strings.Contains(got, "TAG232207") {
		t.Errorf("the prompt should still be trimmed away: %q", got)
	}
}

// TestAfterEcho covers the second silent-truncation path. afterEcho decides
// where a command's output WINDOW STARTS, so getting it wrong discards the
// first lines of output with no trace at all.
func TestAfterEcho(t *testing.T) {
	cases := []struct {
		name string
		// echo is what the terminal emits for the injected command line,
		// including whatever terminates it; out is the command's real output.
		echo string
		out  string
	}{
		{
			name: "posix shell terminates the echo with a newline",
			echo: "echo hi\r\n",
			out:  "hi\r\n",
		},
		{
			name: "conpty terminates the echo with a cursor move",
			echo: "echo AISH-START & ver\x1b[7;1H",
			out:  "AISH-START \x1b[8;1HMicrosoft Windows\r\n",
		},
		{
			name: "conpty echo followed by a relative descent",
			echo: "dir\x1b[2B",
			out:  "Volume in drive C\r\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			trm := term.NewTerminal(24, 80)
			injectedAt := trm.Ring.End()
			trm.Write([]byte(tc.echo))
			trm.Write([]byte(tc.out))

			start := afterEcho(trm, injectedAt)
			got, _, _ := trm.Ring.ReadFrom(start, 1<<16)

			if string(got) != tc.out {
				t.Errorf("output window began in the wrong place:\n got %q\nwant %q", got, tc.out)
			}
		})
	}
}

// TestAfterEchoConptyLosesNothing pins the specific regression: with a '\n'
// scan, a ConPTY echo terminated by CUP made the window start after the first
// newline inside the OUTPUT, eating its first line.
func TestAfterEchoConptyLosesNothing(t *testing.T) {
	const (
		echo  = "echo one & echo two\x1b[5;1H"
		line1 = "one\r\n"
		line2 = "two\r\n"
	)
	trm := term.NewTerminal(24, 80)
	injectedAt := trm.Ring.End()
	trm.Write([]byte(echo + line1 + line2))

	got, _, _ := trm.Ring.ReadFrom(afterEcho(trm, injectedAt), 1<<16)
	if !strings.Contains(string(got), "one") {
		t.Errorf("first output line was eaten by the echo skip: %q", got)
	}
	if !strings.Contains(string(got), "two") {
		t.Errorf("second output line missing: %q", got)
	}
	if strings.Contains(string(got), "echo one") {
		t.Errorf("the echoed command line should have been skipped: %q", got)
	}
}

func TestDropTrailingPrompt(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		timedOut bool
		want     string
	}{
		{"prompt after output", "a\nb\nuser@host:~$ ", false, "a\nb\n"},
		{"already newline terminated", "a\n", false, "a\n"},
		{"prompt only", "user@host:~$ ", false, ""},
		{"empty", "", false, ""},
		{"partial output kept when we gave up", "half a line", true, "half a line"},
		{"timeout still trims a complete line", "a\nhalf", true, "a\n"},
		{"blank lines preserved", "a\n\n\n", false, "a\n\n\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dropTrailingPrompt(tc.in, tc.timedOut); got != tc.want {
				t.Errorf("dropTrailingPrompt(%q, %v) = %q, want %q", tc.in, tc.timedOut, got, tc.want)
			}
		})
	}
}

func TestClampOutput(t *testing.T) {
	t.Run("short output is untouched", func(t *testing.T) {
		in := "just a little output\n"
		got, truncated := clampOutput(in)
		if got != in || truncated {
			t.Errorf("clampOutput(short) = (%q, %v), want (%q, false)", got, truncated, in)
		}
	})

	t.Run("oversized output is bounded and flagged", func(t *testing.T) {
		// Linearization can inflate a window past maxReturn even though its
		// ring range fit — this is the case stripping could never produce.
		in := strings.Repeat("line of output\n", maxReturn/4)
		got, truncated := clampOutput(in)
		if !truncated {
			t.Fatal("expected truncated = true")
		}
		if len(got) > maxReturn+256 { // + the notice itself
			t.Errorf("clamped output still %d bytes, cap is %d", len(got), maxReturn)
		}
		if !strings.Contains(got, "use read_output with cursor to fetch") {
			t.Errorf("truncation notice missing from %q...", got[:80])
		}
		if !strings.HasPrefix(got, "line of output\n") {
			t.Error("head of the output was not preserved")
		}
		if !strings.HasSuffix(got, "line of output\n") {
			t.Error("tail of the output was not preserved")
		}
	})

	t.Run("clamping never splits a rune", func(t *testing.T) {
		// Multi-byte runes straddling both cut points.
		in := strings.Repeat("héllo wörld ✓\n", maxReturn/8)
		got, truncated := clampOutput(in)
		if !truncated {
			t.Fatal("expected truncated = true")
		}
		if !utf8.ValidString(got) {
			t.Error("clamped output is not valid UTF-8")
		}
	})
}

func TestLimitHeadTail(t *testing.T) {
	const s = "héllo" // 6 bytes: h, é (2 bytes), l, l, o

	t.Run("head backs off to a rune boundary", func(t *testing.T) {
		// Byte 2 is the middle of é, so limitHead(2) must back off to 1.
		if got := limitHead(s, 2); got != "h" {
			t.Errorf("limitHead(%q, 2) = %q, want %q", s, got, "h")
		}
	})
	t.Run("tail advances to a rune boundary", func(t *testing.T) {
		// Keeping the last 4 bytes would start mid-é, so it must advance.
		if got := limitTail(s, 4); got != "llo" {
			t.Errorf("limitTail(%q, 4) = %q, want %q", s, got, "llo")
		}
	})
	t.Run("no-op when already short enough", func(t *testing.T) {
		if got := limitHead(s, 99); got != s {
			t.Errorf("limitHead over-length = %q, want %q", got, s)
		}
		if got := limitTail(s, 99); got != s {
			t.Errorf("limitTail over-length = %q, want %q", got, s)
		}
	})
}
