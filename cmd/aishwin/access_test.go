package main

import "testing"

// TestAccessStateEnviron confirms the piece the manual menu e2e test
// couldn't reach cleanly (a FIFO test-harness timing artifact stopped the
// stdin reader goroutine, not a bug in the code): that a menu-set env var
// actually reaches exec.Cmd.Env in the "KEY=VALUE" form it requires, on top
// of the base environment, without disturbing unrelated entries.
func TestAccessStateEnviron(t *testing.T) {
	a := newAccessState()
	base := []string{"PATH=C:\\Windows", "HOME=C:\\Users\\mike"}

	if got := a.environ(base); len(got) != len(base) {
		t.Fatalf("environ with no custom vars = %v, want exactly base %v", got, base)
	}

	a.setEnv("FOO", "bar")
	got := a.environ(base)
	if len(got) != len(base)+1 {
		t.Fatalf("environ after setEnv = %v, want %d entries", got, len(base)+1)
	}
	if got[len(base)] != "FOO=bar" {
		t.Errorf("appended entry = %q, want %q", got[len(base)], "FOO=bar")
	}
	for i, v := range base {
		if got[i] != v {
			t.Errorf("base entry %d = %q, want unchanged %q", i, got[i], v)
		}
	}

	if !a.unsetEnv("FOO") {
		t.Error("unsetEnv(\"FOO\") = false, want true")
	}
	if got := a.environ(base); len(got) != len(base) {
		t.Errorf("environ after unsetEnv = %v, want back to base %v", got, base)
	}
	if a.unsetEnv("FOO") {
		t.Error("unsetEnv(\"FOO\") a second time = true, want false (already gone)")
	}
}
