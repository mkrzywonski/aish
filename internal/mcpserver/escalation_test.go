package mcpserver

import "testing"

func TestEscalationCommand(t *testing.T) {
	cases := []struct {
		name    string
		command string
		want    string // "" means not flagged
	}{
		// --- must be caught ---
		{"bare sudo", "sudo systemctl restart nginx", "sudo"},
		{"sudo with flags", "sudo -u postgres psql -c 'select 1'", "sudo"},
		{"absolute path", "/usr/bin/sudo dnf update", "sudo"},
		{"after cd", "cd /tmp && sudo make install", "sudo"},
		{"after semicolon", "echo starting; sudo reboot", "sudo"},
		{"in a pipeline", "cat list.txt | sudo tee /etc/hosts", "sudo"},
		{"with env assignment", "DEBIAN_FRONTEND=noninteractive sudo apt-get -y upgrade", "sudo"},
		{"inside a subshell", "(sudo dnf clean all)", "sudo"},
		{"su", "su - root -c whoami", "su"},
		{"doas", "doas pkg upgrade", "doas"},
		{"pkexec", "pkexec /usr/bin/id", "pkexec"},
		{"runuser", "runuser -u postgres -- psql", "runuser"},
		{"newline separated", "id\nsudo id", "sudo"},
		{"backgrounded", "sudo long-job &", "sudo"},

		// --- must NOT be caught: sudo appears, but is not being invoked ---
		{"sudo as grep pattern", "grep -rn sudo /etc/pam.d", ""},
		{"reading the sudo log", "tail -n 50 /var/log/sudo.log", ""},
		{"sudo in a quoted string", `echo "run it with; sudo make install"`, ""},
		{"sudo in single quotes", `printf '%s\n' 'sudo is not allowed here'`, ""},
		{"sudoers file path", "cat /etc/sudoers.d/99-custom", ""},
		{"word containing sudo", "ls /opt/pseudosudo/bin", ""},
		{"checking for sudo", "command -v sudo", ""},
		{"ordinary command", "systemctl status nginx", ""},
		{"empty", "", ""},
		{"env assignments only", "FOO=bar BAZ=qux", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, found := escalationCommand(tc.command)
			if tc.want == "" {
				if found {
					t.Errorf("escalationCommand(%q) flagged %q, want no match", tc.command, got)
				}
				return
			}
			if !found {
				t.Fatalf("escalationCommand(%q) found nothing, want %q", tc.command, tc.want)
			}
			if got != tc.want {
				t.Errorf("escalationCommand(%q) = %q, want %q", tc.command, got, tc.want)
			}
		})
	}
}

func TestShellSegmentsRespectsQuotes(t *testing.T) {
	segs := shellSegments(`echo "a; b" && ls`)
	if len(segs) != 2 {
		t.Fatalf("got %d segments %q, want 2", len(segs), segs)
	}
	if segs[0] != `echo "a; b" ` {
		t.Errorf("first segment = %q", segs[0])
	}
}

// TestExecRefusesEscalationOutOfBand checks the guard where it actually
// matters: an out-of-band route refuses, while the visible in-band route is
// left alone, because that one types into the shared terminal where the human
// sees the command and enters their own password.
func TestExecRefusesEscalationOutOfBand(t *testing.T) {
	c := localOOBCore(t)

	_, _, err := c.execTool(t.Context(), nil, execArgs{Command: "sudo id"})
	if err == nil {
		t.Fatal("expected sudo to be refused on the local out-of-band route")
	}
	for _, want := range []string{"sudo", "run_command", "invisibly"} {
		if !contains(err.Error(), want) {
			t.Errorf("refusal should mention %q: %v", want, err)
		}
	}

	// A command that merely mentions sudo still runs.
	if _, _, err := c.execTool(t.Context(), nil, execArgs{Command: "echo 'sudo not used here'"}); err != nil {
		t.Errorf("a command merely mentioning sudo should not be refused: %v", err)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
