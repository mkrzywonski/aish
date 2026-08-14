package mcpserver

import "strings"

// Detecting privilege escalation in a command line.
//
// aish's central promise is that a privileged command is one the human saw:
// sudo prompts stay in the shared terminal, the human types their own password,
// and the AI never handles the secret. An `exec` routed out of band bypasses
// all of that — with NOPASSWD it runs silently, and without it the command just
// fails, because the persistent channel has no TTY and its stdin is /dev/null.
//
// This is a GUARDRAIL, not a security boundary, in the same sense as the tool
// annotations: `bash -c 'sudo ...'`, an aliased wrapper, or a script that
// escalates internally all get through. The goal is to redirect a model that
// reached for sudo out of habit — the common case — not to contain one trying
// to evade the check. Anything running as the user's uid could escalate by
// other means anyway.

var escalationPrograms = map[string]bool{
	"sudo":    true,
	"su":      true,
	"doas":    true,
	"pkexec":  true,
	"runuser": true,
}

// escalationCommand reports whether command invokes a privilege-escalation
// program, and which one. It inspects the first word of each shell segment, so
// `grep -rn sudo /etc` is not flagged (sudo is an argument there) while
// `cd /tmp && sudo make install` is.
func escalationCommand(command string) (string, bool) {
	for _, seg := range shellSegments(command) {
		// Strip grouping and redirection noise that can precede a command word.
		seg = strings.TrimLeft(seg, "({ \t\r\n")
		fields := strings.Fields(seg)

		// Skip leading VAR=value assignments, which legally precede a command.
		i := 0
		for i < len(fields) && isEnvAssignment(fields[i]) {
			i++
		}
		if i >= len(fields) {
			continue
		}

		prog := fields[i]
		if j := strings.LastIndexByte(prog, '/'); j >= 0 {
			prog = prog[j+1:] // /usr/bin/sudo counts too
		}
		if escalationPrograms[prog] {
			return prog, true
		}
	}
	return "", false
}

// shellSegments splits a command line on the operators that begin a new
// command, while respecting quotes — so `echo "a; sudo b"` stays one segment
// and is not mistaken for an escalation.
func shellSegments(s string) []string {
	var (
		segs  []string
		cur   strings.Builder
		quote byte // 0, '\'' or '"'
	)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			cur.WriteByte(c)
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
			cur.WriteByte(c)
		case ';', '\n', '&', '|':
			// Consume the second character of && and ||.
			if (c == '&' || c == '|') && i+1 < len(s) && s[i+1] == c {
				i++
			}
			segs = append(segs, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	segs = append(segs, cur.String())
	return segs
}

// isEnvAssignment reports whether f looks like NAME=value.
func isEnvAssignment(f string) bool {
	eq := strings.IndexByte(f, '=')
	if eq <= 0 {
		return false
	}
	for i := 0; i < eq; i++ {
		c := f[i]
		if !(c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (i > 0 && c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}
