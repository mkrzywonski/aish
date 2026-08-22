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
}

var settings = &appSettings{scrollbackLines: defaultScrollbackLines}

func init() {
	if v, ok := registryGetDWORD(regValueScrollbackLines); ok && v > 0 {
		settings.scrollbackLines = int(v)
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
