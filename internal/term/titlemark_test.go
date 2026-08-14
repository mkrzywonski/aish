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
