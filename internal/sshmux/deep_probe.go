package sshmux

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	deepProbeTimeout   = 60 * time.Second
	deepProbeOutputCap = 8 << 10
)

type DeepProbeStatus string

const (
	DeepProbeIdentified DeepProbeStatus = "identified"
	DeepProbeUnknown    DeepProbeStatus = "unknown"
	DeepProbeFailed     DeepProbeStatus = "failed"
)

// DeepProbeResult is the cached outcome of one explicitly requested identity
// command. Every outcome is cached, including unknown and failed, because each
// attempt opens an SSH session and may trigger MFA.
type DeepProbeResult struct {
	Status      DeepProbeStatus
	Dialect     Dialect
	Platform    string
	Evidence    string
	Reason      string
	Exit        int
	Attempts    int
	AttemptedAt time.Time
	Cached      bool
}

func (r DeepProbeResult) Attempted() bool { return r.Status != "" }

type deepCommandResult struct {
	Stdout   []byte
	Stderr   []byte
	Exit     int
	TimedOut bool
	Err      error
}

type deepCommandRunner func(context.Context, *ConnInfo, string) deepCommandResult

type deepFlight struct {
	done   chan struct{}
	result DeepProbeResult
}

func buildDeepProbeCommand() (command, marker string, err error) {
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return "", "", err
	}
	marker = "AISH_DIALECT_" + hex.EncodeToString(nonce)
	command = "echo " + marker +
		" PCTOS=%OS% PCTCOMSPEC=%COMSPEC% PSOS=$env:OS PSCOMSPEC=$env:ComSpec SH=$SHELL"
	return command, marker, nil
}

// classifyDeepProbe identifies the command grammar from which variable forms
// expanded. It deliberately does not trust environment values such as
// Windows_NT: those can be absent or overridden, while expansion syntax is the
// property being measured.
func classifyDeepProbe(stdout []byte, marker string) (Dialect, string, string, string) {
	fields := strings.Fields(string(stdout))
	start := -1
	for i, field := range fields {
		if field == marker {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return DialectUnknown, "", "", "the active identity marker was not returned"
	}

	values := map[string]string{}
	for _, field := range fields[start:] {
		key, value, ok := strings.Cut(field, "=")
		if _, exists := values[key]; ok && !exists {
			values[key] = value
		}
	}
	for _, key := range []string{"PCTOS", "PCTCOMSPEC", "PSOS", "PSCOMSPEC", "SH"} {
		if _, ok := values[key]; !ok {
			return DialectUnknown, "", "", "the active identity response was incomplete"
		}
	}

	percentLiteral := values["PCTOS"] == "%OS%" && values["PCTCOMSPEC"] == "%COMSPEC%"
	powerShellLiteral := values["PSOS"] == "$env:OS" && values["PSCOMSPEC"] == "$env:ComSpec"
	posixExpansion := values["PSOS"] == ":OS" && values["PSCOMSPEC"] == ":ComSpec"

	switch {
	case !percentLiteral && powerShellLiteral && values["SH"] == "$SHELL":
		return DialectCmd, "windows", "percent variables expanded while PowerShell and POSIX variables remained literal", ""
	case percentLiteral && !powerShellLiteral && !posixExpansion && values["SH"] != "$SHELL":
		return DialectPowerShell, "windows", "PowerShell environment variables expanded while percent variables remained literal", ""
	case percentLiteral && posixExpansion && values["SH"] != "$SHELL":
		return DialectPosix, "unix", "POSIX variable expansion consumed $env while percent variables remained literal", ""
	default:
		return DialectUnknown, "", "", "the active identity expansion pattern was not recognized"
	}
}

func (m *Mux) runDeepCommand(parent context.Context, ci *ConnInfo, command string) deepCommandResult {
	ctx, cancel := context.WithTimeout(parent, deepProbeTimeout)
	defer cancel()

	stdout := &boundedBuf{cap: deepProbeOutputCap}
	stderr := &boundedBuf{cap: deepProbeOutputCap}
	cmd := m.Command(ctx, ci, command)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	exit := -1
	if cmd.ProcessState != nil {
		exit = cmd.ProcessState.ExitCode()
	}
	return deepCommandResult{
		Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), Exit: exit,
		TimedOut: errors.Is(ctx.Err(), context.DeadlineExceeded), Err: err,
	}
}

func executeDeepProbe(ctx context.Context, ci *ConnInfo, run deepCommandRunner) DeepProbeResult {
	result := DeepProbeResult{Exit: -1, AttemptedAt: time.Now()}
	command, marker, err := buildDeepProbeCommand()
	if err != nil {
		result.Status = DeepProbeFailed
		result.Reason = "could not generate the active identity marker"
		return result
	}

	observed := run(ctx, ci, command)
	result.Exit = observed.Exit
	dialect, platform, evidence, reason := classifyDeepProbe(observed.Stdout, marker)
	if dialect != DialectUnknown {
		result.Status = DeepProbeIdentified
		result.Dialect = dialect
		result.Platform = platform
		result.Evidence = evidence
		return result
	}

	switch {
	case observed.TimedOut:
		result.Status = DeepProbeFailed
		result.Reason = fmt.Sprintf("the active identity probe timed out (maximum duration %s)", deepProbeTimeout)
	case observed.Err != nil:
		result.Status = DeepProbeFailed
		result.Reason = "the active identity command failed"
		if line := firstLine(string(observed.Stderr)); line != "" {
			result.Reason += ": " + line
		} else if observed.Exit >= 0 {
			result.Reason += fmt.Sprintf(" with exit status %d", observed.Exit)
		}
	default:
		result.Status = DeepProbeUnknown
		result.Reason = reason
	}
	return result
}

// DeepProbe runs at most one active identity command per target at a time.
// Cached outcomes return without opening a session; force discards only the deep
// attempt and deep-derived identity. Waiting callers share the in-flight result.
func (m *Mux) DeepProbe(ctx context.Context, ci *ConnInfo, force bool) (DeepProbeResult, error) {
	if ci == nil || ci.Sock == "" {
		return DeepProbeResult{}, errors.New("active identity probing requires a ControlMaster target")
	}

	m.deepMu.Lock()
	if flight := m.deepFlights[ci.Sock]; flight != nil {
		m.deepMu.Unlock()
		select {
		case <-flight.done:
			result := flight.result
			result.Cached = true
			return result, nil
		case <-ctx.Done():
			return DeepProbeResult{}, ctx.Err()
		}
	}
	if force {
		m.forgetDeepFacts(ci)
	} else if result, ok := m.CachedDeepProbe(ci); ok {
		m.deepMu.Unlock()
		result.Cached = true
		return result, nil
	}
	flight := &deepFlight{done: make(chan struct{})}
	m.deepFlights[ci.Sock] = flight
	m.deepMu.Unlock()

	result := executeDeepProbe(ctx, ci, m.deepRun)
	result = m.noteDeepProbe(ci, result)

	m.deepMu.Lock()
	flight.result = result
	delete(m.deepFlights, ci.Sock)
	close(flight.done)
	m.deepMu.Unlock()
	return result, nil
}
