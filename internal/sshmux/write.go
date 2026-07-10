package sshmux

import (
	"fmt"
	"strings"
)

// Exit codes the atomic write subshell reports so the caller can turn them into
// specific errors. They sit above any code a normal command would return here.
const (
	WriteExitWriteFail = 91 // temp file could not be written
	WriteExitStale     = 92 // if_match did not match the current file (CAS lost)
	WriteExitSymlink   = 93 // target is a symlink; we refuse to follow/replace it
)

// WriteRequest describes one atomic, optionally conditional write over the OOB
// channel.
type WriteRequest struct {
	Path string
	Data []byte
	// Mode is an explicit octal mode ("0644"); empty preserves the existing
	// file's mode, or defaults to 644 for a new file.
	Mode string
	// IfMatch, when set, makes the write a compare-and-swap: the rename only
	// happens if the current file still matches this version token. It is
	// "sha256:<hex>" or "mtime-size:<mtime>:<size>".
	IfMatch string
	// Hasher is the remote tool used to verify a sha256 IfMatch: "sha256sum" or
	// "shasum". Ignored for mtime-size or unconditional writes.
	Hasher string
}

// AtomicWriteScript builds a POSIX sh script that installs Data at Path
// atomically: it writes to a temp file in Path's own directory, preserves the
// file's mode (or applies Mode), refuses to follow a symlink, optionally checks
// an if_match version, and renames into place (POSIX rename is atomic within a
// filesystem). All logic runs in a subshell so a failure exits only the
// subshell — never the persistent channel's `sh -s` — and the subshell's exit
// code (0, or one of the WriteExit* codes) is what the channel sentinel
// reports. The base64 heredoc is always consumed so channel framing stays
// intact even on the error paths.
func AtomicWriteScript(req WriteRequest) string {
	p := Quote(req.Path)
	var b strings.Builder
	b.WriteString("(\n")
	fmt.Fprintf(&b, "_p=%s\n", p)
	// A temp file next to the target keeps the final rename intra-filesystem
	// (hence atomic). Fall back to a fixed name when mktemp is unavailable.
	b.WriteString(`_tmp=$(mktemp "$(dirname "$_p")/.aishtmp.XXXXXX" 2>/dev/null) || _tmp="$_p.aishtmp.$$"` + "\n")
	b.WriteString("base64 -d > \"$_tmp\" <<'@AISH_EOF@'\n")
	b.WriteString(wrap76(req.Data))
	b.WriteString("\n@AISH_EOF@\n")
	fmt.Fprintf(&b, "[ $? = 0 ] || { rm -f \"$_tmp\"; exit %d; }\n", WriteExitWriteFail)
	fmt.Fprintf(&b, "[ -L \"$_p\" ] && { rm -f \"$_tmp\"; exit %d; }\n", WriteExitSymlink)

	// Mode: explicit wins; else preserve the existing file's (GNU or BSD stat);
	// else default 644 for a brand-new file.
	if m := sanitizeMode(req.Mode); m != "" {
		fmt.Fprintf(&b, "chmod %s \"$_tmp\" 2>/dev/null\n", m)
	} else {
		b.WriteString(`if _m=$(stat -c %a "$_p" 2>/dev/null || stat -f %Lp "$_p" 2>/dev/null); then chmod "$_m" "$_tmp" 2>/dev/null; else chmod 644 "$_tmp" 2>/dev/null; fi` + "\n")
	}

	// Compare-and-swap on the version token, if requested.
	if cas := casBlock(req); cas != "" {
		b.WriteString(cas)
	}

	b.WriteString("mv -f \"$_tmp\" \"$_p\"\n")
	b.WriteString(")")
	return b.String()
}

// casBlock builds the "verify current version, else abort" fragment. The token
// is validated and single-quoted, so it can't inject shell.
func casBlock(req WriteRequest) string {
	if req.IfMatch == "" {
		return ""
	}
	var cur string
	switch {
	case strings.HasPrefix(req.IfMatch, "sha256:"):
		hasher := "sha256sum"
		if req.Hasher == "shasum" {
			hasher = "shasum -a 256"
		}
		cur = fmt.Sprintf(`_cur=sha256:$(%s < "$_p" 2>/dev/null | cut -c1-64)`, hasher)
	case strings.HasPrefix(req.IfMatch, "mtime-size:"):
		cur = `_cur=mtime-size:$(stat -c '%Y:%s' "$_p" 2>/dev/null || stat -f '%m:%z' "$_p" 2>/dev/null)`
	default:
		return ""
	}
	return cur + "\n" +
		fmt.Sprintf("[ \"$_cur\" = %s ] || { rm -f \"$_tmp\"; exit %d; }\n", Quote(req.IfMatch), WriteExitStale)
}

// sanitizeMode returns s only if it is a plain 3-4 digit octal mode, else "".
// Guards the mode against shell injection when embedded in the script.
func sanitizeMode(s string) string {
	if len(s) < 3 || len(s) > 4 {
		return ""
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '7' {
			return ""
		}
	}
	return s
}
