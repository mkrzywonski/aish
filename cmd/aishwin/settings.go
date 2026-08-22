package main

// settings.go: persistent user settings, backed by the registry
// (registry.go) so they survive across aishwin restarts. New settings
// should follow scrollbackLines's shape: a field here, a getter/setter
// pair, and a registry value name -- then a settingsField entry in
// gui_settings.go's buildSettingsFields to expose it in the Settings
// window.

import (
	"fmt"
	"sync"
)

const defaultScrollbackLines = 5000

const regValueScrollbackLines = "ScrollbackLines"

// Connection settings (Settings > Connection): which of the two spawn
// paths (spawn.go) a plain `aishwin.exe` with no --ssh/--wsl flag uses.
const (
	connModeWSL    = "wsl"
	connModeSSH    = "ssh"
	defaultSSHPort = 22

	regValueConnectionMode = "ConnectionMode"
	regValueSSHHost        = "SSHHost"
	regValueSSHPort        = "SSHPort"
	regValueSSHUser        = "SSHUser"
)

// registryKeyPath is where settings are persisted. A dev build
// (aishwindev tag) uses a separate subkey so testing this feature can
// never overwrite a real user's persisted settings.
var registryKeyPath = func() string {
	if devBuild {
		return `Software\aishwin-dev`
	}
	return `Software\aishwin`
}()

type appSettings struct {
	mu              sync.Mutex
	scrollbackLines int
	connectionMode  string
	sshHost         string
	sshPort         int
	sshUser         string
}

var settings = &appSettings{
	scrollbackLines: defaultScrollbackLines,
	connectionMode:  connModeWSL,
	sshPort:         defaultSSHPort,
}

func init() {
	if v, ok := registryGetDWORD(regValueScrollbackLines); ok && v > 0 {
		settings.scrollbackLines = int(v)
	}
	if v, ok := registryGetString(regValueConnectionMode); ok && (v == connModeWSL || v == connModeSSH) {
		settings.connectionMode = v
	}
	if v, ok := registryGetString(regValueSSHHost); ok {
		settings.sshHost = v
	}
	if v, ok := registryGetDWORD(regValueSSHPort); ok && v > 0 && v < 65536 {
		settings.sshPort = int(v)
	}
	if v, ok := registryGetString(regValueSSHUser); ok {
		settings.sshUser = v
	}
}

func (s *appSettings) ScrollbackLines() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scrollbackLines
}

func (s *appSettings) SetScrollbackLines(n int) {
	s.mu.Lock()
	s.scrollbackLines = n
	s.mu.Unlock()
	if err := registrySetDWORD(regValueScrollbackLines, uint32(n)); err != nil {
		AppendLog(fmt.Sprintf("aishwin: failed to save scrollback setting: %v", err))
	}
}

// ConnectionMode is connModeWSL or connModeSSH.
func (s *appSettings) ConnectionMode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connectionMode
}

func (s *appSettings) SetConnectionMode(mode string) {
	s.mu.Lock()
	s.connectionMode = mode
	s.mu.Unlock()
	if err := registrySetString(regValueConnectionMode, mode); err != nil {
		AppendLog(fmt.Sprintf("aishwin: failed to save connection mode: %v", err))
	}
}

func (s *appSettings) SSHHost() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sshHost
}

func (s *appSettings) SetSSHHost(host string) {
	s.mu.Lock()
	s.sshHost = host
	s.mu.Unlock()
	if err := registrySetString(regValueSSHHost, host); err != nil {
		AppendLog(fmt.Sprintf("aishwin: failed to save SSH host: %v", err))
	}
}

func (s *appSettings) SSHPort() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sshPort
}

func (s *appSettings) SetSSHPort(port int) {
	s.mu.Lock()
	s.sshPort = port
	s.mu.Unlock()
	if err := registrySetDWORD(regValueSSHPort, uint32(port)); err != nil {
		AppendLog(fmt.Sprintf("aishwin: failed to save SSH port: %v", err))
	}
}

func (s *appSettings) SSHUser() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sshUser
}

func (s *appSettings) SetSSHUser(user string) {
	s.mu.Lock()
	s.sshUser = user
	s.mu.Unlock()
	if err := registrySetString(regValueSSHUser, user); err != nil {
		AppendLog(fmt.Sprintf("aishwin: failed to save SSH user: %v", err))
	}
}
