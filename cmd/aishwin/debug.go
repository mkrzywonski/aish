//go:build windows

package main

import (
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"
)

// debugEnabled is toggled by the --debug CLI flag. All debug output goes
// through debugLog, which renders in the GUI log view (AppendLog) so it is
// visible even when there is no attached console.
var debugEnabled atomic.Bool

const colorDebug = 0x00008080 // dark yellow/olive -- distinct from file (blue) and command (red)

// debugLog writes a timestamped debug line to the GUI log view when
// --debug is active. Safe to call from any goroutine, before or after the
// GUI exists (AppendLog queues until the window is created).
func debugLog(format string, args ...any) {
	if !debugEnabled.Load() {
		return
	}
	msg := fmt.Sprintf(format, args...)
	AppendLogColor(fmt.Sprintf("[DEBUG %s] %s", time.Now().Format("15:04:05.000"), msg), colorDebug)
}

func mustMarshal(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
