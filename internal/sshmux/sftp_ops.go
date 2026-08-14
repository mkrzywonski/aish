package sshmux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/pkg/sftp"
)

// ErrSFTPClientDead marks a retained subsystem that cannot serve more
// operations. Callers must explicitly force a new probe; operations never
// reopen it because doing so may trigger MFA.
var ErrSFTPClientDead = errors.New("retained SFTP client is unavailable")

// SFTPFileInfo is transport-neutral metadata returned by the retained client.
type SFTPFileInfo struct {
	Path    string
	Name    string
	Size    int64
	Mode    os.FileMode
	ModTime time.Time
}

// SFTPReadResult is a bounded file slice and its canonical target-native path.
type SFTPReadResult struct {
	Path string
	Data []byte
	EOF  bool
}

// SFTPDirectoryResult is a bounded direct-child listing.
type SFTPDirectoryResult struct {
	Path      string
	Entries   []SFTPFileInfo
	Truncated bool
}

// sftpOperations is deliberately narrower than pkg/sftp.Client. Tests can
// exercise retained-client death and routing without exposing that dependency
// above the mux boundary.
type sftpOperations interface {
	Read(string, int64, int) ([]byte, bool, error)
	Lstat(string) (SFTPFileInfo, error)
	ReadDir(context.Context, string) ([]SFTPFileInfo, error)
	Download(string, io.Writer) (int64, error)
	Close() error
}

type pkgSFTPOperations struct {
	client *sftp.Client
}

func (o *pkgSFTPOperations) Read(path string, offset int64, max int) ([]byte, bool, error) {
	f, err := o.client.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()

	buf := make([]byte, max+1)
	n, err := f.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, err
	}
	eof := n <= max
	if n > max {
		n = max
	}
	return buf[:n], eof, nil
}

func (o *pkgSFTPOperations) Lstat(path string) (SFTPFileInfo, error) {
	info, err := o.client.Lstat(path)
	if err != nil {
		return SFTPFileInfo{}, err
	}
	return sftpFileInfo(info), nil
}

func (o *pkgSFTPOperations) ReadDir(ctx context.Context, path string) ([]SFTPFileInfo, error) {
	infos, err := o.client.ReadDirContext(ctx, path)
	if err != nil {
		return nil, err
	}
	entries := make([]SFTPFileInfo, 0, len(infos))
	for _, info := range infos {
		entries = append(entries, sftpFileInfo(info))
	}
	return entries, nil
}

func (o *pkgSFTPOperations) Download(path string, dst io.Writer) (int64, error) {
	f, err := o.client.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return io.Copy(dst, f)
}

func (o *pkgSFTPOperations) Close() error { return o.client.Close() }

func sftpFileInfo(info os.FileInfo) SFTPFileInfo {
	return SFTPFileInfo{
		Name: info.Name(), Size: info.Size(), Mode: info.Mode(), ModTime: info.ModTime(),
	}
}

type sftpOperationResult[T any] struct {
	value T
	err   error
}

func runSFTPOperation[T any](ctx context.Context, session *sftpSession, host string, op func(sftpOperations) (T, error)) (T, error) {
	var zero T
	session.opMu.Lock()
	defer session.opMu.Unlock()

	if session.dead {
		return zero, sftpDeadError(host, nil)
	}
	if session.processDone != nil {
		select {
		case <-session.processDone:
			session.dead = true
			session.shutdownLocked()
			return zero, sftpDeadError(host, session.processErr)
		default:
		}
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}

	done := make(chan sftpOperationResult[T], 1)
	go func() {
		value, err := op(session.ops)
		done <- sftpOperationResult[T]{value: value, err: err}
	}()

	select {
	case result := <-done:
		if sftpTransportError(result.err) {
			session.dead = true
			session.shutdownLocked()
			return zero, sftpDeadError(host, result.err)
		}
		return result.value, result.err
	case <-ctx.Done():
		session.dead = true
		session.shutdownLocked()
		return zero, sftpDeadError(host, ctx.Err())
	}
}

func sftpTransportError(err error) bool {
	if err == nil {
		return false
	}
	var status *sftp.StatusError
	if errors.As(err, &status) {
		return status.FxCode() == sftp.ErrSshFxNoConnection || status.FxCode() == sftp.ErrSshFxConnectionLost
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, os.ErrClosed) {
		return true
	}
	text := strings.ToLower(err.Error())
	for _, phrase := range []string{"connection lost", "connection reset", "broken pipe", "unexpectedly closed", "use of closed network connection"} {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

func sftpDeadError(host string, cause error) error {
	detail := ""
	if cause != nil {
		detail = ": " + cause.Error()
	}
	return fmt.Errorf("%w for %s%s; it will not be reopened automatically. Retry only with probe_host using sftp=true and force=true; opening a new subsystem may trigger MFA", ErrSFTPClientDead, host, detail)
}

func (m *Mux) retainedSFTP(ci *ConnInfo) (*sftpSession, SftpAxis, error) {
	if ci == nil || ci.Sock == "" {
		return nil, SftpAxis{}, errors.New("SFTP operations require a ControlMaster target")
	}
	facts, ok := m.Facts(ci)
	if !ok || facts.SFTP.State != AxisUp {
		reason := facts.SFTP.Reason
		if reason == "" {
			reason = "the SFTP subsystem has not been opened successfully"
		}
		return nil, facts.SFTP, sftpDeadError(ci.Host, errors.New(reason))
	}
	m.sftpMu.Lock()
	session := m.sftpSessions[ci.Sock]
	m.sftpMu.Unlock()
	if session == nil || session.ops == nil {
		return nil, facts.SFTP, sftpDeadError(ci.Host, errors.New("no retained client exists for the cached SFTP capability"))
	}
	return session, facts.SFTP, nil
}

func (m *Mux) retireSFTP(ci *ConnInfo, session *sftpSession, cause error) {
	m.sftpMu.Lock()
	wasCurrent := m.sftpSessions[ci.Sock] == session
	if wasCurrent {
		delete(m.sftpSessions, ci.Sock)
	}
	m.sftpMu.Unlock()
	session.Close()
	if !wasCurrent {
		// A concurrent forced probe already detached this generation. It owns
		// the facts now; an old operation must not mark its replacement down.
		return
	}

	m.factsMu.Lock()
	if facts := m.facts[ci.Sock]; facts != nil && facts.SFTP.State == AxisUp {
		facts.SFTP.State = AxisDown
		facts.SFTP.Reason = "the retained SFTP client was lost"
		if cause != nil {
			facts.SFTP.Reason += ": " + cause.Error()
		}
	}
	m.factsMu.Unlock()
}

// SFTPRead reads at most max bytes without opening a new subsystem.
func (m *Mux) SFTPRead(ctx context.Context, ci *ConnInfo, input string, offset int64, max int) (SFTPReadResult, error) {
	if offset < 0 {
		return SFTPReadResult{}, errors.New("offset must not be negative")
	}
	if max <= 0 {
		return SFTPReadResult{}, errors.New("max bytes must be positive")
	}
	session, axis, err := m.retainedSFTP(ci)
	if err != nil {
		return SFTPReadResult{}, err
	}
	resolved, err := normalizeSFTPPath(axis.PathStyle, input)
	if err != nil {
		return SFTPReadResult{}, err
	}
	result, err := runSFTPOperation(ctx, session, ci.Host, func(ops sftpOperations) (SFTPReadResult, error) {
		data, eof, err := ops.Read(resolved.Server, offset, max)
		return SFTPReadResult{Path: resolved.Native, Data: data, EOF: eof}, err
	})
	if errors.Is(err, ErrSFTPClientDead) {
		m.retireSFTP(ci, session, err)
	}
	return result, err
}

// SFTPStat returns link-preserving metadata without opening a new subsystem.
func (m *Mux) SFTPStat(ctx context.Context, ci *ConnInfo, input string) (SFTPFileInfo, error) {
	session, axis, err := m.retainedSFTP(ci)
	if err != nil {
		return SFTPFileInfo{}, err
	}
	resolved, err := normalizeSFTPPath(axis.PathStyle, input)
	if err != nil {
		return SFTPFileInfo{}, err
	}
	result, err := runSFTPOperation(ctx, session, ci.Host, func(ops sftpOperations) (SFTPFileInfo, error) {
		info, err := ops.Lstat(resolved.Server)
		info.Path = resolved.Native
		return info, err
	})
	if errors.Is(err, ErrSFTPClientDead) {
		m.retireSFTP(ci, session, err)
	}
	return result, err
}

// SFTPReadDir returns at most max direct children without opening a new subsystem.
func (m *Mux) SFTPReadDir(ctx context.Context, ci *ConnInfo, input string, max int) (SFTPDirectoryResult, error) {
	if max <= 0 {
		return SFTPDirectoryResult{}, errors.New("max entries must be positive")
	}
	session, axis, err := m.retainedSFTP(ci)
	if err != nil {
		return SFTPDirectoryResult{}, err
	}
	resolved, err := normalizeSFTPPath(axis.PathStyle, input)
	if err != nil {
		return SFTPDirectoryResult{}, err
	}
	result, err := runSFTPOperation(ctx, session, ci.Host, func(ops sftpOperations) (SFTPDirectoryResult, error) {
		entries, err := ops.ReadDir(ctx, resolved.Server)
		result := SFTPDirectoryResult{Path: resolved.Native, Entries: entries, Truncated: len(entries) > max}
		if len(result.Entries) > max {
			result.Entries = result.Entries[:max]
		}
		return result, err
	})
	if errors.Is(err, ErrSFTPClientDead) {
		m.retireSFTP(ci, session, err)
	}
	return result, err
}

// SFTPDownload streams a remote file to dst without opening a new subsystem.
func (m *Mux) SFTPDownload(ctx context.Context, ci *ConnInfo, input string, dst io.Writer) (int64, error) {
	session, axis, err := m.retainedSFTP(ci)
	if err != nil {
		return 0, err
	}
	resolved, err := normalizeSFTPPath(axis.PathStyle, input)
	if err != nil {
		return 0, err
	}
	written, err := runSFTPOperation(ctx, session, ci.Host, func(ops sftpOperations) (int64, error) {
		return ops.Download(resolved.Server, dst)
	})
	if errors.Is(err, ErrSFTPClientDead) {
		m.retireSFTP(ci, session, err)
	}
	return written, err
}
