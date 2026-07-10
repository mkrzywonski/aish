package sshmux

import "strings"

// Capabilities describes what the remote host behind a persistent OOB channel
// can do. It is probed exactly once, as the first op when the channel is
// opened, then cached for the channel's lifetime. session_status reports the
// cached value and never triggers a probe of its own (a probe opens a channel,
// which can cost an MFA push).
//
// The Ok* fields are behavioral tests, not presence checks: BusyBox `stat`
// exists but lacks `--printf`, macOS `base64` wants `-D` not `-d`, so we run
// each tricky option once and record whether it actually worked.
type Capabilities struct {
	OS       string `json:"os,omitempty"`
	Arch     string `json:"arch,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	User     string `json:"user,omitempty"`
	Cwd      string `json:"cwd,omitempty"`

	HasRg     bool   `json:"has_rg"`
	HasGrep   bool   `json:"has_grep"`
	HasFind   bool   `json:"has_find"`
	Hasher    string `json:"hasher"` // sha256sum | shasum | none
	HasMktemp bool   `json:"has_mktemp"`
	PkgMgr    string `json:"pkg_mgr,omitempty"` // apt-get | dnf | yum | apk | pkg | brew | pacman | zypper

	// Behavioral capability tests (see above).
	HasBase64 bool `json:"has_base64"` // base64 exists (encode works)
	Base64D   bool `json:"base64_d"`   // `base64 -d` decodes (GNU/BusyBox)
	Base64Dup bool `json:"base64_dup"` // `base64 -D` decodes (BSD/macOS)
	StatC     bool `json:"stat_c"`     // `stat -c` works (GNU/BusyBox)
	StatF     bool `json:"stat_f"`     // `stat -f` works (BSD/macOS)
	FindPrint bool `json:"find_printf"`
	HeadZ     bool `json:"head_z"`
	GrepNull  bool `json:"grep_null"`

	// Unsupported marks a host whose shell isn't POSIX enough to run the probe.
	// Set by the channel handshake when the shell never returns our sentinel.
	Unsupported bool `json:"unsupported"`
}

// Base64Decode returns the flag this host's base64 uses to decode, or "" when
// no base64 (or no working decode flag) is available.
func (c Capabilities) Base64Decode() string {
	switch {
	case c.Base64D:
		return "-d"
	case c.Base64Dup:
		return "-D"
	default:
		return ""
	}
}

// probeScript emits one labeled key=value line per fact. Labels (not position)
// keep parsing stable when a command is missing: an absent tool yields an empty
// value, never a dropped line that shifts everything after it. Each behavioral
// test runs the real option against a harmless target and echoes 1 on success.
const probeScript = `printf 'uname=%s\n' "$(uname -sm 2>/dev/null)"
printf 'user=%s\n' "$(id -un 2>/dev/null)"
printf 'hostname=%s\n' "$(hostname 2>/dev/null)"
printf 'pwd=%s\n' "$(pwd 2>/dev/null)"
printf 'rg=%s\n' "$(command -v rg 2>/dev/null)"
printf 'grep=%s\n' "$(command -v grep 2>/dev/null)"
printf 'find=%s\n' "$(command -v find 2>/dev/null)"
printf 'sha256sum=%s\n' "$(command -v sha256sum 2>/dev/null)"
printf 'shasum=%s\n' "$(command -v shasum 2>/dev/null)"
printf 'mktemp=%s\n' "$(command -v mktemp 2>/dev/null)"
printf 'pkg=%s\n' "$(command -v apt-get dnf yum apk pkg brew pacman zypper 2>/dev/null | head -n1)"
printf 'base64=%s\n' "$(printf hi | base64 >/dev/null 2>&1 && echo 1)"
printf 'base64d=%s\n' "$(printf aGk= | base64 -d >/dev/null 2>&1 && echo 1)"
printf 'base64D=%s\n' "$(printf aGk= | base64 -D >/dev/null 2>&1 && echo 1)"
printf 'statc=%s\n' "$(stat -c %s / >/dev/null 2>&1 && echo 1)"
printf 'statf=%s\n' "$(stat -f %z / >/dev/null 2>&1 && echo 1)"
printf 'findprintf=%s\n' "$(find / -maxdepth 0 -printf '' >/dev/null 2>&1 && echo 1)"
printf 'headz=%s\n' "$(printf 'a\n' | head -z -n1 >/dev/null 2>&1 && echo 1)"
printf 'grepnull=%s\n' "$(printf x | grep --null -o x >/dev/null 2>&1 && echo 1)"`

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
	c.HasGrep = kv["grep"] != ""
	c.HasFind = kv["find"] != ""
	c.HasMktemp = kv["mktemp"] != ""
	c.PkgMgr = basename(kv["pkg"])
	switch {
	case kv["sha256sum"] != "":
		c.Hasher = "sha256sum"
	case kv["shasum"] != "":
		c.Hasher = "shasum"
	default:
		c.Hasher = "none"
	}
	c.HasBase64 = kv["base64"] == "1"
	c.Base64D = kv["base64d"] == "1"
	c.Base64Dup = kv["base64D"] == "1"
	c.StatC = kv["statc"] == "1"
	c.StatF = kv["statf"] == "1"
	c.FindPrint = kv["findprintf"] == "1"
	c.HeadZ = kv["headz"] == "1"
	c.GrepNull = kv["grepnull"] == "1"
	if c.OS == "" {
		c.Unsupported = true
	}
	return c
}

func basename(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// EnsureProbed makes sure ci's channel is open and its capability probe has
// run, returning the cached capabilities. Opening the channel is the same
// action (and same possible MFA push) the caller's own OOB op is about to
// trigger, so this adds no extra authorization cost — it just moves the probe
// ahead of a divergence check so even the first write can be verified against
// the real remote host. A ":" no-op forces the open+probe without side effects.
func (m *Mux) EnsureProbed(ci *ConnInfo) (Capabilities, error) {
	if c, ok := m.CachedCapabilities(ci); ok {
		return c, nil
	}
	if _, err := m.ChannelRun(ci, ":", minOpenTimeout); err != nil {
		return Capabilities{}, err
	}
	c, _ := m.CachedCapabilities(ci)
	return c, nil
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
