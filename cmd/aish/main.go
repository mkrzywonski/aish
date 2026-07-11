// aish is a terminal wrapper that lets a human and an AI agent drive one
// shared shell session. It also masquerades as `ssh` (via a PATH shim
// symlink) to inject ControlMaster options into ssh invocations made from
// inside a session.
package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"ai-ssh/internal/debugcli"
	"ai-ssh/internal/framing"
	"ai-ssh/internal/mcpserver"
	"ai-ssh/internal/paths"
	"ai-ssh/internal/proxy"
	"ai-ssh/internal/session"
	"ai-ssh/internal/shellintegration"
	"ai-ssh/internal/sshmux"
	"ai-ssh/internal/state"
	"ai-ssh/internal/term"
)

const usage = `aish — AI-shareable terminal

Usage:
  aish [run] [--name <name>] [--oob] [--no-auth] [--auto-approve]
                      start a shared shell session (default)
                      --oob authorizes out-of-band (invisible) file/exec
                      operations; without it every AI action goes through
                      the shared terminal where you can see it
                      --no-auth skips the y/n prompt on each new client
                      connection (zero-friction; you won't be asked)
                      --auto-approve keeps the auth handshake but auto-answers
                      the approval prompt (for one-shot testing; you're
                      notified but not blocked)
  aish sessions       list live sessions (id, name)
  aish install [claude|codex|all]
                      register the aish MCP server with an AI TUI (default: all found)
  aish uninstall [claude|codex|all]
                      remove the aish MCP server from an AI TUI
  aish mcp-proxy      stdio<->socket MCP proxy for AI agents (run by the TUI)
  aish client [--session <id|name>] <tool> [json-args]
                      call an MCP tool on a running session (debug)
  aish version
`

var version = "0.2.0-dev"

func main() {
	// Busybox-style dispatch: when invoked through the PATH shim symlink
	// named "ssh", act as the ssh wrapper.
	if filepath.Base(os.Args[0]) == "ssh" {
		os.Exit(sshmux.ShimMain(os.Args[1:]))
	}

	args := os.Args[1:]
	sub := "run"
	if len(args) > 0 && args[0][0] != '-' {
		sub = args[0]
		args = args[1:]
	}

	switch sub {
	case "run":
		os.Exit(runMain(args))
	case "sessions":
		os.Exit(sessionsMain())
	case "install":
		os.Exit(installMain(args))
	case "uninstall":
		os.Exit(uninstallMain(args))
	case "mcp-proxy":
		os.Exit(proxy.Main(version, args))
	case "client":
		os.Exit(debugcli.Main(version, args))
	case "version":
		fmt.Println("aish", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "aish: unknown command %q\n%s", sub, usage)
		os.Exit(2)
	}
}

func runMain(args []string) int {
	var name string
	var oob bool
	var noAuth bool
	var autoApprove bool
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--name":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "aish: --name requires a value")
				return 2
			}
			name = args[i+1]
			i++
		case "--oob":
			oob = true
		case "--no-auth":
			noAuth = true
		case "--auto-approve":
			autoApprove = true
		default:
			fmt.Fprintf(os.Stderr, "aish: unknown option %q\n%s", args[i], usage)
			return 2
		}
	}
	if name != "" && !paths.ValidName(name) {
		fmt.Fprintln(os.Stderr, "aish: invalid session name (letters, digits, . _ -, max 32 chars, must start alphanumeric)")
		return 2
	}

	if os.Getenv("AISH_SESSION") != "" {
		fmt.Fprintln(os.Stderr, "aish: already inside an aish session (AISH_SESSION is set)")
		return 1
	}

	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}

	sweepStaleSessions()

	id := session.NewID()
	dir := paths.SessionDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "aish:", err)
		return 1
	}
	defer os.RemoveAll(dir)
	sock := paths.Socket(id)
	if name != "" {
		if err := paths.WriteName(id, name); err != nil {
			fmt.Fprintln(os.Stderr, "aish:", err)
			return 1
		}
	}

	if oob {
		if err := paths.GrantOOB(id); err != nil {
			fmt.Fprintln(os.Stderr, "aish:", err)
			return 1
		}
	}

	argv, shellEnv, err := shellintegration.Setup(shell, dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aish: shell integration disabled: %v\n", err)
		argv, shellEnv = []string{shell}, nil
	}

	extraEnv := append([]string{
		"AISH_SESSION=" + id,
		"AISH_SOCKET=" + sock,
		"AISH_DIR=" + dir,
	}, shellEnv...)

	// Install the ssh PATH shim: a symlink to this binary named "ssh",
	// first on PATH inside the session, so every ssh invocation gains
	// ControlMaster multiplexing.
	shimBin := paths.ShimBin(id)
	if exe, err := os.Executable(); err == nil {
		if err := os.MkdirAll(shimBin, 0o700); err == nil {
			if err := os.Symlink(exe, filepath.Join(shimBin, "ssh")); err == nil {
				extraEnv = append(extraEnv,
					"PATH="+shimBin+":"+os.Getenv("PATH"),
					"AISH_SHIM_BIN="+shimBin)
			}
		}
	}

	sess := session.New(id, argv, extraEnv)
	titles := term.NewTitleMarker(os.Stdout)
	sess.Stdout = titles
	label := name
	if label == "" {
		label = id
	}
	titles.SetLabel(label) // also badges the title immediately, before any shell output

	trm := term.NewTerminal(24, 80)
	sess.AddTap(trm)
	sess.OnResize(func(rows, cols uint16) {
		trm.Screen.Resize(int(rows), int(cols))
	})

	tracker := state.NewTracker(func() int {
		if sess.Ptmx == nil {
			return -1
		}
		return int(sess.Ptmx.Fd())
	})
	go tracker.Consume(trm.Parser.Subscribe())

	engine := &framing.Engine{Sess: sess, Term: trm, Tracker: tracker}

	mux := sshmux.New(dir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	core := &mcpserver.Core{
		Sess: sess, Term: trm, Tracker: tracker, Engine: engine,
		Mux: mux, Tasks: sshmux.NewTable(), Version: version, NoAuth: noAuth, AutoApprove: autoApprove,
		OnClients: func(n int) { titles.SetConnected(n > 0) },
		OnRenamed: func(newName string) { titles.SetLabel(newName) },
	}

	// Status bar: a reserved bottom row showing the session badge, the tty host
	// aish believes it's on (OSC 7 — stale is itself a signal), and the OOB
	// target (user@host), so host/identity divergence is visible at a glance.
	// Painted from aish's own state, so it works even when the remote is silent.
	// A poll drives it and reflects host drift in the title ⚠ (no intrusive
	// notice — the bar is the durable signal).
	bar := session.NewStatusBar(sess, core.StatusLine, func() bool { return trm.Screen.Snapshot().AltScreen })
	defer bar.Close() // reset the scroll region on a normal return
	sess.OnResize(func(rows, cols uint16) { bar.SetSize(rows, cols) })
	go func() {
		tick := time.NewTicker(500 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				mismatch, _, _ := core.HostDrift()
				titles.SetDrift(mismatch)
				bar.Tick()
			}
		}
	}()

	// Ctrl-] opens the aish menu: rename this session or toggle out-of-band
	// operations. The prompt badge re-reads the name file on the next prompt;
	// SetLabel updates the window title immediately. OOB toggling flips both
	// the in-process grant and the persisted marker the ssh shim reads.
	sess.SetMenu(func() {
		oobState := "off"
		if core.OOBEnabled() {
			oobState = "on"
		}
		accept := "rok"
		lines := []string{
			"aish menu:",
			"  [r] rename session",
			fmt.Sprintf("  [o] out-of-band ops (currently %s)", oobState),
			"  [k] revoke client access",
		}
		// Offer the aish prompt (badge + host tracking) only on a remote whose
		// host aish can't yet verify.
		if trackHost, ok := core.RemoteTrackingApplicable(); ok {
			accept += "p"
			lines = append(lines, fmt.Sprintf("  [p] set up aish prompt on %s", trackHost))
		}
		lines = append(lines, "  Esc to cancel")
		choice, ok := sess.Prompt(strings.Join(lines, "\r\n"), accept, 30*time.Second)
		if !ok {
			return
		}
		switch choice {
		case 'r':
			newName, ok := sess.PromptLine("new session name:", 60*time.Second)
			if !ok {
				return
			}
			newName = strings.TrimSpace(newName)
			if !paths.ValidName(newName) {
				sess.Notify("invalid name (letters, digits, . _ -, max 32 chars, must start alphanumeric)")
				return
			}
			if err := paths.WriteName(id, newName); err != nil {
				sess.Notify("rename failed: %v", err)
				return
			}
			titles.SetLabel(newName)
			sess.Notify("session renamed to %q", newName)
		case 'o':
			on := !core.OOBEnabled()
			core.SetOOB(on)
			if on {
				sess.Notify("out-of-band operations ENABLED — the AI may now act invisibly (ControlMaster channels, direct file/exec)")
			} else {
				sess.Notify("out-of-band operations DISABLED — AI activity stays visible in the shared terminal")
			}
		case 'k':
			n := core.Revoke()
			sess.Notify("revoked client access — %d connection(s) dropped; all clients cleared and must be re-approved", n)
			if noAuth {
				sess.Notify("note: this session runs with --no-auth, so reconnecting clients are NOT prompted for approval")
			}
		case 'p':
			core.ProvisionRemoteTracking()
		}
	})

	go func() {
		if err := mcpserver.Serve(ctx, core, sock); err != nil {
			fmt.Fprintln(os.Stderr, "aish: mcp server:", err)
		}
	}()

	// The deferred cleanup only runs on a normal return; make external
	// termination (window closed → SIGHUP, kill) clean up too.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sig
		bar.Close() // reset the scroll region before we exit without running defers
		mux.CloseAll()
		os.RemoveAll(dir)
		os.Exit(1)
	}()

	code, err := sess.Run()
	mux.CloseAll()
	if err != nil {
		fmt.Fprintln(os.Stderr, "aish:", err)
	}
	return code
}

// sessionsMain lists live sessions for humans picking a --session target.
func sessionsMain() int {
	live := proxy.List()
	if len(live) == 0 {
		fmt.Println("no live aish sessions")
		return 0
	}
	for _, s := range live {
		fmt.Printf("%-10s %s\n", s.ID, s.Name)
	}
	return 0
}

// sweepStaleSessions removes runtime dirs of sessions whose MCP socket no
// longer answers (left behind by SIGKILL or a crash).
func sweepStaleSessions() {
	entries, err := os.ReadDir(paths.Base())
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sock := paths.Socket(e.Name())
		conn, err := net.Dial("unix", sock)
		if err == nil {
			conn.Close()
			continue
		}
		// No answering socket. If the socket file doesn't exist yet this may
		// be a session that is starting up right now (dir created, listener
		// not yet bound) — only sweep it once the dir is old enough.
		if _, serr := os.Stat(sock); serr != nil {
			if info, ierr := e.Info(); ierr == nil && time.Since(info.ModTime()) < time.Minute {
				continue
			}
		}
		os.RemoveAll(paths.SessionDir(e.Name()))
	}
}
