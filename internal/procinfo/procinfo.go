// Package procinfo reads human-readable identity for a local process id from
// /proc, used to describe who is on the other end of a connection (the
// kernel-verified peer of a session's Unix socket) and who launched the proxy.
// It is best-effort and Linux-oriented: on failure it returns "".
package procinfo

import (
	"os"
	"strconv"
	"strings"
)

// Name returns a short label for pid: its comm (e.g. "aish"), enriched with the
// basename of argv[0] when that differs (e.g. a wrapper). Returns "" when pid
// can't be read.
func Name(pid int) string {
	if pid <= 0 {
		return ""
	}
	comm := readComm(pid)
	arg0 := readArg0(pid)
	switch {
	case comm == "" && arg0 == "":
		return ""
	case arg0 == "" || arg0 == comm:
		return comm
	case comm == "":
		return arg0
	default:
		return comm + " (" + arg0 + ")"
	}
}

func readComm(pid int) string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// readArg0 returns the basename of the process's argv[0] (NUL-delimited cmdline).
func readArg0(pid int) string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil || len(b) == 0 {
		return ""
	}
	arg0 := string(b)
	if i := strings.IndexByte(arg0, 0); i >= 0 {
		arg0 = arg0[:i]
	}
	if i := strings.LastIndexByte(arg0, '/'); i >= 0 {
		arg0 = arg0[i+1:]
	}
	return strings.TrimSpace(arg0)
}
