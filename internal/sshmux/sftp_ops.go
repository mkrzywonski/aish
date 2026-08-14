package sshmux

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"strings"
	"time"

	"github.com/pkg/sftp"
)

// ErrSFTPClientDead marks a retained subsystem that cannot serve more
// operations. Callers must explicitly force a new probe; operations never
// reopen it because doing so may trigger MFA.
var ErrSFTPClientDead = errors.New("retained SFTP client is unavailable")

var (
	ErrSFTPAtomicReplaceUnsupported = errors.New("SFTP server does not advertise atomic replacement")
	ErrSFTPWriteStale               = errors.New("SFTP destination changed since it was read")
	ErrSFTPWriteSymlink             = errors.New("refusing to replace an SFTP symlink")
	ErrSFTPWriteNoVersion           = errors.New("SFTP destination version could not be verified")
	ErrSFTPWriteMode                = errors.New("SFTP server did not apply the required file mode")
)

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

// SFTPWriteRequest preserves the existing atomic-write contract over a
// retained SFTP client. ModeSet distinguishes an explicit mode from the zero
// value; otherwise an existing mode is preserved and a new file uses 0644.
type SFTPWriteRequest struct {
	Path    string
	Data    []byte
	Mode    os.FileMode
	ModeSet bool
	IfMatch string
}

type SFTPWriteResult struct {
	Path  string
	Bytes int
}

// sftpOperations is deliberately narrower than pkg/sftp.Client. Tests can
// exercise retained-client death and routing without exposing that dependency
// above the mux boundary.
type sftpOperations interface {
	Read(string, int64, int) ([]byte, bool, error)
	Lstat(string) (SFTPFileInfo, error)
	ReadDir(context.Context, string) ([]SFTPFileInfo, error)
	Download(string, io.Writer) (int64, error)
	WriteExclusive(string, []byte) error
	Append(string, []byte) error
	Chmod(string, os.FileMode) error
	Remove(string) error
	PosixRename(string, string) error
	SHA256(string) (string, error)
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

func (o *pkgSFTPOperations) WriteExclusive(path string, data []byte) error {
	f, err := o.client.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return err
	}
	n, writeErr := f.Write(data)
	if writeErr == nil && n != len(data) {
		writeErr = io.ErrShortWrite
	}
	return errors.Join(writeErr, f.Close())
}

func (o *pkgSFTPOperations) Append(path string, data []byte) error {
	f, err := o.client.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND)
	if err != nil {
		return err
	}
	n, writeErr := f.Write(data)
	if writeErr == nil && n != len(data) {
		writeErr = io.ErrShortWrite
	}
	return errors.Join(writeErr, f.Close())
}

func (o *pkgSFTPOperations) Chmod(path string, mode os.FileMode) error {
	return o.client.Chmod(path, mode)
}

func (o *pkgSFTPOperations) Remove(path string) error {
	return o.client.Remove(path)
}

func (o *pkgSFTPOperations) PosixRename(oldPath, newPath string) error {
	return o.client.PosixRename(oldPath, newPath)
}

func (o *pkgSFTPOperations) SHA256(path string) (string, error) {
	f, err := o.client.Open(path)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, copyErr := io.Copy(h, f)
	if err := errors.Join(copyErr, f.Close()); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
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

// SFTPWriteAtomic replaces a regular file through an exclusive temporary file
// in the destination directory. It requires posix-rename@openssh.com because
// ordinary SFTP v3 rename cannot atomically overwrite an existing destination.
func (m *Mux) SFTPWriteAtomic(ctx context.Context, ci *ConnInfo, req SFTPWriteRequest) (SFTPWriteResult, error) {
	session, axis, err := m.retainedSFTP(ci)
	if err != nil {
		return SFTPWriteResult{}, err
	}
	resolved, err := normalizeSFTPPath(axis.PathStyle, req.Path)
	if err != nil {
		return SFTPWriteResult{}, err
	}
	if !containsString(axis.Extensions, "posix-rename@openssh.com") {
		return SFTPWriteResult{}, fmt.Errorf("%w; refusing non-atomic remove-and-rename fallback", ErrSFTPAtomicReplaceUnsupported)
	}
	tmpPath, err := sftpTempPath(resolved.Server)
	if err != nil {
		return SFTPWriteResult{}, err
	}

	result, err := runSFTPOperation(ctx, session, ci.Host, func(ops sftpOperations) (SFTPWriteResult, error) {
		initial, initialExists, err := sftpDestinationInfo(ops, resolved.Server)
		if err != nil {
			return SFTPWriteResult{}, err
		}
		if initialExists && initial.Mode&os.ModeSymlink != 0 {
			return SFTPWriteResult{}, ErrSFTPWriteSymlink
		}

		mode := os.FileMode(0o644)
		if req.ModeSet {
			mode = req.Mode.Perm()
		} else if initialExists {
			mode = initial.Mode.Perm()
		}
		if err := ops.WriteExclusive(tmpPath, req.Data); err != nil {
			return SFTPWriteResult{}, err
		}
		cleanup := func(primary error) error {
			removeErr := ops.Remove(tmpPath)
			if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				removeErr = fmt.Errorf("cleaning SFTP temporary file: %w", removeErr)
			} else {
				removeErr = nil
			}
			return errors.Join(primary, removeErr)
		}
		if err := ops.Chmod(tmpPath, mode); err != nil {
			return SFTPWriteResult{}, cleanup(fmt.Errorf("setting SFTP temporary file mode: %w", err))
		}
		if req.ModeSet || initialExists {
			tmpInfo, err := ops.Lstat(tmpPath)
			if err != nil {
				return SFTPWriteResult{}, cleanup(fmt.Errorf("verifying SFTP temporary file mode: %w", err))
			}
			if tmpInfo.Mode.Perm() != mode {
				return SFTPWriteResult{}, cleanup(fmt.Errorf("%w: requested %04o, server reported %04o", ErrSFTPWriteMode, mode, tmpInfo.Mode.Perm()))
			}
		}

		current, currentExists, err := sftpDestinationInfo(ops, resolved.Server)
		if err != nil {
			return SFTPWriteResult{}, cleanup(err)
		}
		if currentExists && current.Mode&os.ModeSymlink != 0 {
			return SFTPWriteResult{}, cleanup(ErrSFTPWriteSymlink)
		}
		if req.IfMatch != "" {
			version, err := sftpVersion(ops, resolved.Server, current, currentExists, req.IfMatch)
			if err != nil {
				return SFTPWriteResult{}, cleanup(err)
			}
			if version != req.IfMatch {
				return SFTPWriteResult{}, cleanup(ErrSFTPWriteStale)
			}
		}
		if err := ops.PosixRename(tmpPath, resolved.Server); err != nil {
			return SFTPWriteResult{}, cleanup(fmt.Errorf("atomic SFTP rename failed: %w", err))
		}
		return SFTPWriteResult{Path: resolved.Native, Bytes: len(req.Data)}, nil
	})
	if errors.Is(err, ErrSFTPClientDead) {
		m.retireSFTP(ci, session, err)
	}
	return result, err
}

// SFTPAppend appends directly, matching the existing non-atomic append
// contract. An explicit mode is applied after the write.
func (m *Mux) SFTPAppend(ctx context.Context, ci *ConnInfo, input string, data []byte, mode os.FileMode, modeSet bool) (SFTPWriteResult, error) {
	session, axis, err := m.retainedSFTP(ci)
	if err != nil {
		return SFTPWriteResult{}, err
	}
	resolved, err := normalizeSFTPPath(axis.PathStyle, input)
	if err != nil {
		return SFTPWriteResult{}, err
	}
	result, err := runSFTPOperation(ctx, session, ci.Host, func(ops sftpOperations) (SFTPWriteResult, error) {
		if err := ops.Append(resolved.Server, data); err != nil {
			return SFTPWriteResult{}, err
		}
		if modeSet {
			if err := ops.Chmod(resolved.Server, mode.Perm()); err != nil {
				return SFTPWriteResult{}, fmt.Errorf("setting appended SFTP file mode: %w", err)
			}
		}
		return SFTPWriteResult{Path: resolved.Native, Bytes: len(data)}, nil
	})
	if errors.Is(err, ErrSFTPClientDead) {
		m.retireSFTP(ci, session, err)
	}
	return result, err
}

func sftpDestinationInfo(ops sftpOperations, filePath string) (SFTPFileInfo, bool, error) {
	info, err := ops.Lstat(filePath)
	if err == nil {
		return info, true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return SFTPFileInfo{}, false, nil
	}
	return SFTPFileInfo{}, false, err
}

func sftpVersion(ops sftpOperations, filePath string, info SFTPFileInfo, exists bool, token string) (string, error) {
	if !exists {
		return "", ErrSFTPWriteNoVersion
	}
	switch {
	case strings.HasPrefix(token, "sha256:"):
		version, err := ops.SHA256(filePath)
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrSFTPWriteNoVersion, err)
		}
		return version, nil
	case strings.HasPrefix(token, "mtime-size:"):
		return fmt.Sprintf("mtime-size:%d:%d", info.ModTime.Unix(), info.Size), nil
	default:
		return "", errors.New("unsupported if_match token; use a version from file_read or file_stat")
	}
}

func sftpTempPath(destination string) (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("creating SFTP temporary name: %w", err)
	}
	return path.Join(path.Dir(destination), ".aishtmp."+hex.EncodeToString(random[:])), nil
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
