package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"ai-ssh/internal/aishwinwire"
)

var (
	errStaleWrite   = errors.New("the file changed since it was read (if_match mismatch); re-read it and retry")
	errSymlinkWrite = errors.New("refusing to write through a symlink; write to the symlink's real target path instead")
)

type dirEntry struct {
	Name         string
	Type         string
	Size         int64
	ModifiedUnix int64
}

// readFile reads up to max bytes of path starting at offset, mirroring
// aish's own local file_read path (internal/mcpserver/tools_remote.go).
// eof reports whether the read reached (or was already at) the end of the
// file. Go's os package is already cross-platform, so this algorithm needs
// no Windows-specific handling.
func readFile(path string, offset int64, max int) (data []byte, eof bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	// A directory opens cleanly and then fails on the first Read — an error
	// this function used to discard, so reading a folder returned empty
	// content with eof set, indistinguishable from a genuinely empty file
	// (down to a valid sha256 of the empty string). Refuse it by name, and
	// point at the tool that does answer the question.
	if info, statErr := f.Stat(); statErr == nil && info.IsDir() {
		return nil, false, fmt.Errorf("%s is a directory, not a file; use directory_list to see what it contains", path)
	}
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return nil, false, err
		}
	}
	buf := make([]byte, max+1)
	n, readErr := readFullBuf(f, buf)
	// A short read ends in io.EOF, which is ordinary. Any other failure has
	// to reach the caller rather than being reported as empty content.
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, false, readErr
	}
	eof = n <= max
	if n > max {
		n = max
	}
	return buf[:n], eof, nil
}

func readFullBuf(f *os.File, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := f.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// statFile mirrors aish's local file_stat: Lstat, so it reports on a
// symlink itself rather than following it.
func statFile(path string) (kind, mode string, size, modifiedUnix int64, err error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", "", 0, 0, err
	}
	return fileType(info.Mode()), fmt.Sprintf("%04o", info.Mode().Perm()), info.Size(), info.ModTime().Unix(), nil
}

// listDir mirrors aish's local directory_list. os.ReadDir already returns
// entries sorted by name.
func listDir(path string, max int) (entries []dirEntry, truncated bool, err error) {
	all, err := os.ReadDir(path)
	if err != nil {
		return nil, false, err
	}
	truncated = len(all) > max
	if truncated {
		all = all[:max]
	}
	for _, e := range all {
		info, err := e.Info()
		if err != nil {
			return nil, false, fmt.Errorf("stat %s: %w", e.Name(), err)
		}
		entries = append(entries, dirEntry{
			Name: e.Name(), Type: fileType(info.Mode()), Size: info.Size(), ModifiedUnix: info.ModTime().Unix(),
		})
	}
	return entries, truncated, nil
}

// writeFileAtomic installs data at path atomically (temp file in the same
// directory + rename), preserving the existing mode (or applying mode),
// refusing to follow a symlink, and — when ifMatch is set — swapping only
// if the current file still matches that version token. Mirrors aish's own
// atomicWriteLocal (internal/mcpserver/tools_remote.go): the algorithm is
// pure os-package Go, already cross-platform, and os.Rename on Windows
// atomically replaces an existing destination (since Go 1.5), so the same
// temp+rename approach applies unchanged here.
func writeFileAtomic(path string, data []byte, mode, ifMatch string) error {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return errSymlinkWrite
	}
	if ifMatch != "" {
		cur, err := fileVersion(path, ifMatch)
		if err != nil {
			return err
		}
		if cur != ifMatch {
			return errStaleWrite
		}
	}
	perm := os.FileMode(0o644)
	if m, ok, err := parseOptionalMode(mode); ok && err == nil {
		perm = m
	} else if fi, err := os.Stat(path); err == nil {
		perm = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".aishwintmp.*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename below succeeds
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Best-effort on Windows: there are no POSIX permission bits, so
	// os.Chmod only toggles the read-only attribute based on the owner-write
	// bit. Still worth applying for parity with aish's own contract.
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// appendFile mirrors aish's append path: not atomic, just O_APPEND.
func appendFile(path string, data []byte, mode string) (int, error) {
	perm := os.FileMode(0o644)
	if m, ok, err := parseOptionalMode(mode); ok && err == nil {
		perm = m
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, perm)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return f.Write(data)
}

func parseOptionalMode(s string) (os.FileMode, bool, error) {
	if s == "" {
		return 0, false, nil
	}
	var m uint32
	if _, err := fmt.Sscanf(s, "%o", &m); err != nil {
		return 0, false, err
	}
	return os.FileMode(m).Perm(), true, nil
}

// fileVersion computes path's current version token in the same kind as
// tokenKind (sha256 or mtime-size), mirroring aish's localVersion.
func fileVersion(path, tokenKind string) (string, error) {
	switch {
	case strings.HasPrefix(tokenKind, "sha256:"):
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return aishwinwire.SHA256Version(data), nil
	case strings.HasPrefix(tokenKind, "mtime-size:"):
		fi, err := os.Stat(path)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("mtime-size:%d:%d", fi.ModTime().Unix(), fi.Size()), nil
	}
	return "", errors.New("unsupported if_match token; use a version from file_read or file_stat")
}

// fileType categorizes a file's mode. Windows lacks POSIX device
// files/fifos/sockets in the ordinary filesystem namespace, so this is
// simpler than aish's own localFileType but shares its shape.
func fileType(mode os.FileMode) string {
	switch {
	case mode.IsDir():
		return "directory"
	case mode&os.ModeSymlink != 0:
		return "symlink"
	case mode.IsRegular():
		return "file"
	default:
		return "other"
	}
}
