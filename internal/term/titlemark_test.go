package term

import (
	"bytes"
	"strings"
	"testing"
)

func TestTitleMarkerMirrorsPending2FAState(t *testing.T) {
	var out bytes.Buffer
	marker := NewTitleMarker(&out)
	marker.SetLabel("test")
	out.Reset()

	marker.SetAuthPending(true)
	if !strings.Contains(out.String(), "⧉test[2FA?]") {
		t.Fatalf("pending title = %q", out.String())
	}
	out.Reset()
	marker.SetAuthPending(false)
	if strings.Contains(out.String(), "[2FA?]") {
		t.Fatalf("restored title retained pending marker: %q", out.String())
	}
}

func TestTitleMarkerMirrorsPendingInputState(t *testing.T) {
	var out bytes.Buffer
	marker := NewTitleMarker(&out)
	marker.SetLabel("test")
	out.Reset()

	marker.SetInputPending(true)
	if !strings.Contains(out.String(), "⧉test[INPUT?]") {
		t.Fatalf("pending title = %q", out.String())
	}
	out.Reset()
	marker.SetInputPending(false)
	if strings.Contains(out.String(), "[INPUT?]") {
		t.Fatalf("restored title retained input marker: %q", out.String())
	}
}

func TestTitleMarkerCanShowInputAnd2FA(t *testing.T) {
	var out bytes.Buffer
	marker := NewTitleMarker(&out)
	marker.SetLabel("test")
	marker.SetAuthPending(true)
	out.Reset()
	marker.SetInputPending(true)
	if got := out.String(); !strings.Contains(got, "[2FA?][INPUT?]") {
		t.Fatalf("combined pending title = %q", got)
	}
}

func TestTitleMarkerRefreshDoesNotEmitBEL(t *testing.T) {
	var out bytes.Buffer
	marker := NewTitleMarker(&out)
	marker.SetLabel("test")
	if strings.ContainsRune(out.String(), '\a') {
		t.Fatalf("generated title refresh used BEL terminator: %q", out.String())
	}
	if !strings.HasSuffix(out.String(), "\x1b\\") {
		t.Fatalf("generated title refresh lacks ST terminator: %q", out.String())
	}
}
