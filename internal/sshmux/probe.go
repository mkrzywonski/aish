package sshmux

import "strings"

// Capabilities describes what the remote host behind a persistent OOB channel
// can do. It is probed exactly once, as the first op when the channel is
// opened, then cached for the channel's lifetime. session_status reports the
// cached value and never triggers a probe of its own (a probe opens a channel,
// which can cost an MFA push).
type Capabilities struct {
	OS         string `json:"os,omitempty"`
	Arch       string `json:"arch,omitempty"`
	Hostname   string `json:"hostname,omitempty"`
	User       string `json:"user,omitempty"`
	Cwd        string `json:"cwd,omitempty"`
	HasRg      bool   `json:"has_rg"`
	Hasher     string `json:"hasher"`      // sha256sum | shasum | none
	GrepFlavor string `json:"grep_flavor"` // gnu | other
	HasMktemp  bool   `json:"has_mktemp"`
	// Unsupported marks a host whose shell isn't POSIX enough to probe (empty
	// uname): the native-style primitives should refuse rather than emit
	// garbage. Covers Windows/appliance targets reached over ssh.
	Unsupported bool `json:"unsupported"`
}

// probeScript emits one labeled key=value line per fact. Labels (not position)
// keep parsing stable when a command is missing: an absent tool yields an empty
// value, never a dropped line that shifts everything after it.
const probeScript = `printf 'uname=%s\n' "$(uname -sm 2>/dev/null)"
printf 'user=%s\n' "$(id -un 2>/dev/null)"
printf 'hostname=%s\n' "$(hostname 2>/dev/null)"
printf 'pwd=%s\n' "$(pwd 2>/dev/null)"
printf 'rg=%s\n' "$(command -v rg 2>/dev/null)"
printf 'sha256sum=%s\n' "$(command -v sha256sum 2>/dev/null)"
printf 'shasum=%s\n' "$(command -v shasum 2>/dev/null)"
printf 'mktemp=%s\n' "$(command -v mktemp 2>/dev/null)"
printf 'grep_version=%s\n' "$(grep --version 2>/dev/null | head -1)"`

func parseCapabilities(out []byte) Capabilities {
	kv := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(strings.TrimRight(line, "\r"), "="); ok {
			kv[k] = v
		}
	}
	var c Capabilities
	if f := strings.Fields(kv["uname"]); len(f) > 0 {
		c.OS = f[0]
		if len(f) > 1 {
			c.Arch = f[1]
		}
	}
	c.Hostname = kv["hostname"]
	c.User = kv["user"]
	c.Cwd = kv["pwd"]
	c.HasRg = kv["rg"] != ""
	c.HasMktemp = kv["mktemp"] != ""
	switch {
	case kv["sha256sum"] != "":
		c.Hasher = "sha256sum"
	case kv["shasum"] != "":
		c.Hasher = "shasum"
	default:
		c.Hasher = "none"
	}
	// Match "GNU grep" specifically: GNU prints "grep (GNU grep) 3.7", while BSD
	// prints "grep (BSD grep, GNU compatible)" — and BSD find lacks -printf, so
	// treating it as non-GNU (no -Z / find -printf) is the conservative choice.
	// BusyBox prints nothing useful for --version, so it falls here too.
	if strings.Contains(kv["grep_version"], "GNU grep") {
		c.GrepFlavor = "gnu"
	} else {
		c.GrepFlavor = "other"
	}
	if c.OS == "" {
		c.Unsupported = true
	}
	return c
}

// probeChannel runs the capability probe as the first op on a freshly opened
// channel and caches the result. Best-effort: on any failure caps stays unset.
func (m *Mux) probeChannel(ch *channel) {
	res, err := ch.run(probeScript, minOpenTimeout)
	if err != nil || res == nil || res.TimedOut {
		return
	}
	c := parseCapabilities(res.Output)
	ch.caps.Store(&c)
}

// CachedCapabilities returns the probed capabilities for ci's channel when the
// channel is already open and probed. It never opens a channel or runs a
// command, so session_status can call it without risking an MFA push. ok is
// false when nothing has been probed yet (report the host as unknown then).
func (m *Mux) CachedCapabilities(ci *ConnInfo) (Capabilities, bool) {
	if ci == nil || ci.Sock == "" {
		return Capabilities{}, false
	}
	m.chMu.Lock()
	ch := m.channels[ci.Sock]
	m.chMu.Unlock()
	if ch == nil {
		return Capabilities{}, false
	}
	if c := ch.caps.Load(); c != nil {
		return *c, true
	}
	return Capabilities{}, false
}
