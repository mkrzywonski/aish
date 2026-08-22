package main

// connection.go: lets something other than main() (the Settings dialog's
// Connect button) replace the running connection with a freshly chosen
// spawnFunc, without restarting the whole process. main() still owns the
// process-lifetime context (set once via InitConnectionContext); each
// StartConnection call layers a cancelable child context over it so a
// later call can tear down exactly the connection attempt it started
// (spawnFunc's *exec.Cmd is created via exec.CommandContext, so canceling
// kills the underlying wsl.exe/ssh process, which unblocks run()'s read
// loop and cmd.Wait() the same way a real disconnect would) without
// touching the app's own shutdown signal.

import (
	"context"
	"sync"
)

var (
	appCtx context.Context

	connMu     sync.Mutex
	connCancel context.CancelFunc

	// distroFlagValue/requestedSessionName capture the --distro/--name CLI
	// flags once at startup (main.go) so a later WSL reconnect (from
	// resolveSpawnFromSettings, e.g. the Settings dialog's Connect button)
	// keeps using them without threading them through as parameters
	// everywhere; they never change after startup.
	distroFlagValue      string
	requestedSessionName string
)

// resolveSpawn picks how to launch the Linux half for the very first
// connection at process startup: an explicit --ssh or --wsl flag always
// wins (overriding, but not overwriting, whatever's persisted), otherwise
// it follows the Connection settings saved in the registry (Settings >
// Connection), so a plain `aishwin.exe` with no flags reconnects the same
// way it was last configured.
func resolveSpawn(sshTargetFlag string, forceWSL bool) spawnFunc {
	if sshTargetFlag != "" {
		return spawnSSH(sshTargetFlag)
	}
	if forceWSL {
		return spawnWSL(distroFlagValue)
	}
	return resolveSpawnFromSettings()
}

// resolveSpawnFromSettings always reflects the current persisted Connection
// settings, ignoring any --ssh/--wsl CLI override -- used by the Settings
// dialog's Connect button, an explicit "connect using what I just
// configured" action that shouldn't be second-guessed by a flag from
// process startup.
func resolveSpawnFromSettings() spawnFunc {
	if settings.ConnectionMode() == connModeSSH && settings.SSHHost() != "" {
		return spawnSSHConfig(settings.SSHHost(), settings.SSHPort(), settings.SSHUser())
	}
	return spawnWSL(distroFlagValue)
}

// InitConnectionContext records the process-lifetime context every
// connection attempt is ultimately scoped under. Called once from main
// before the first StartConnection.
func InitConnectionContext(ctx context.Context) {
	appCtx = ctx
}

// StartConnection cancels whatever connection attempt is currently running
// (a no-op the first time) and starts a new one with spawn/name. Safe to
// call repeatedly -- each call supersedes the last, which is exactly what
// the Settings dialog's Connect button needs after the user changes the
// connection mode/host/port/user.
func StartConnection(spawn spawnFunc, name string) {
	connMu.Lock()
	if connCancel != nil {
		connCancel()
	}
	ctx, cancel := context.WithCancel(appCtx)
	connCancel = cancel
	connMu.Unlock()

	go func() {
		if err := run(ctx, spawn, name); err != nil {
			AppendLog("aishwin: " + err.Error())
		}
	}()
}
