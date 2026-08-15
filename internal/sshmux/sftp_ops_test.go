package sshmux

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeSFTPOperations struct {
	mu            sync.Mutex
	paths         []string
	readData      []byte
	readErr       error
	stat          SFTPFileInfo
	statErr       error
	entries       []SFTPFileInfo
	download      []byte
	lstatFn       func(string) (SFTPFileInfo, error)
	writeFn       func(string, []byte) error
	appendFn      func(string, []byte) error
	chmodFn       func(string, os.FileMode) error
	removeFn      func(string) error
	renameFn      func(string, string) error
	sha256Fn      func(string) (string, error)
	block         <-chan struct{}
	closed        chan struct{}
	closeOnce     sync.Once
	closeCount    atomic.Int32
	active        atomic.Int32
	maxConcurrent atomic.Int32
}

func (f *fakeSFTPOperations) note(path string) {
	f.mu.Lock()
	f.paths = append(f.paths, path)
	f.mu.Unlock()
	active := f.active.Add(1)
	for {
		old := f.maxConcurrent.Load()
		if active <= old || f.maxConcurrent.CompareAndSwap(old, active) {
			break
		}
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-f.closed:
		}
	}
	f.active.Add(-1)
}

func (f *fakeSFTPOperations) Read(path string, offset int64, max int) ([]byte, bool, error) {
	f.note(path)
	data := append([]byte(nil), f.readData...)
	return data, len(data) <= max, f.readErr
}

func (f *fakeSFTPOperations) Lstat(path string) (SFTPFileInfo, error) {
	f.note(path)
	if f.lstatFn != nil {
		return f.lstatFn(path)
	}
	return f.stat, f.statErr
}

func (f *fakeSFTPOperations) ReadDir(ctx context.Context, path string) ([]SFTPFileInfo, error) {
	f.note(path)
	return append([]SFTPFileInfo(nil), f.entries...), nil
}

func (f *fakeSFTPOperations) Download(path string, dst io.Writer) (int64, error) {
	f.note(path)
	n, err := dst.Write(f.download)
	return int64(n), err
}

func (f *fakeSFTPOperations) WriteExclusive(path string, data []byte) error {
	f.note(path)
	if f.writeFn != nil {
		return f.writeFn(path, append([]byte(nil), data...))
	}
	return nil
}

func (f *fakeSFTPOperations) Append(path string, data []byte) error {
	f.note(path)
	if f.appendFn != nil {
		return f.appendFn(path, append([]byte(nil), data...))
	}
	return nil
}

func (f *fakeSFTPOperations) Chmod(path string, mode os.FileMode) error {
	f.note(path)
	if f.chmodFn != nil {
		return f.chmodFn(path, mode)
	}
	return nil
}

func (f *fakeSFTPOperations) Remove(path string) error {
	f.note(path)
	if f.removeFn != nil {
		return f.removeFn(path)
	}
	return nil
}

func (f *fakeSFTPOperations) PosixRename(oldPath, newPath string) error {
	f.note(oldPath + " -> " + newPath)
	if f.renameFn != nil {
		return f.renameFn(oldPath, newPath)
	}
	return nil
}

func (f *fakeSFTPOperations) SHA256(path string) (string, error) {
	f.note(path)
	if f.sha256Fn != nil {
		return f.sha256Fn(path)
	}
	return "", nil
}

func (f *fakeSFTPOperations) Close() error {
	f.closeCount.Add(1)
	f.closeOnce.Do(func() {
		if f.closed != nil {
			close(f.closed)
		}
	})
	return nil
}

func installFakeSFTP(t *testing.T, style string, ops sftpOperations) (*Mux, *ConnInfo) {
	return installFakeSFTPWithExtensions(t, style, nil, ops)
}

func installFakeSFTPWithExtensions(t *testing.T, style string, extensions []string, ops sftpOperations) (*Mux, *ConnInfo) {
	t.Helper()
	m := New(t.TempDir())
	ci := testConn()
	m.sftpRun = func(context.Context, *ConnInfo) (*sftpSession, SftpAxis) {
		return &sftpSession{ops: ops}, SftpAxis{
			State: AxisUp, RealPath: "/home/mike", PathStyle: style,
			Extensions: append([]string(nil), extensions...), ProbedAt: time.Now(),
		}
	}
	if _, err := m.ProbeSFTP(context.Background(), ci, false); err != nil {
		t.Fatal(err)
	}
	return m, ci
}

func TestSFTPRetainedOperationsNormalizePathsAndBoundResults(t *testing.T) {
	ops := &fakeSFTPOperations{
		readData: []byte("hello"),
		stat:     SFTPFileInfo{Name: "file.txt", Size: 5, Mode: 0o640, ModTime: time.Unix(123, 0)},
		entries: []SFTPFileInfo{
			{Name: "one", Size: 1}, {Name: "two", Size: 2}, {Name: "three", Size: 3},
		},
		download: []byte("download"),
	}
	m, ci := installFakeSFTP(t, "windows", ops)

	read, err := m.SFTPRead(context.Background(), ci, `c:\Users\mk31\a b.txt`, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if read.Path != `C:\Users\mk31\a b.txt` || string(read.Data) != "hello" || !read.EOF {
		t.Errorf("read = %+v", read)
	}

	stat, err := m.SFTPStat(context.Background(), ci, "/C:/Users/mk31/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if stat.Path != `C:\Users\mk31\file.txt` || stat.Name != "file.txt" || stat.Mode.Perm() != 0o640 {
		t.Errorf("stat = %+v", stat)
	}

	dir, err := m.SFTPReadDir(context.Background(), ci, `C:\Users\mk31`, 2)
	if err != nil {
		t.Fatal(err)
	}
	if dir.Path != `C:\Users\mk31` || !dir.Truncated || len(dir.Entries) != 2 {
		t.Errorf("directory = %+v", dir)
	}

	var downloaded bytes.Buffer
	n, err := m.SFTPDownload(context.Background(), ci, `C:\Users\mk31\file.txt`, &downloaded)
	if err != nil || n != 8 || downloaded.String() != "download" {
		t.Errorf("download = %d/%q, err=%v", n, downloaded.String(), err)
	}

	ops.mu.Lock()
	paths := append([]string(nil), ops.paths...)
	ops.mu.Unlock()
	want := []string{"/C:/Users/mk31/a b.txt", "/C:/Users/mk31/file.txt", "/C:/Users/mk31", "/C:/Users/mk31/file.txt"}
	if strings.Join(paths, "|") != strings.Join(want, "|") {
		t.Errorf("server paths = %q, want %q", paths, want)
	}
}

func TestSFTPOperationsSerializePerRetainedClient(t *testing.T) {
	block := make(chan struct{})
	ops := &fakeSFTPOperations{readData: []byte("x"), block: block, closed: make(chan struct{})}
	m, ci := installFakeSFTP(t, "posix", ops)

	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := m.SFTPRead(context.Background(), ci, "/tmp/file", 0, 10)
			errs <- err
		}()
	}
	time.Sleep(20 * time.Millisecond)
	close(block)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := ops.maxConcurrent.Load(); got != 1 {
		t.Fatalf("maximum concurrent operations = %d, want 1", got)
	}
}

func TestSFTPTransportLossIsStickyAndNeverReopens(t *testing.T) {
	ops := &fakeSFTPOperations{readErr: io.ErrUnexpectedEOF, closed: make(chan struct{})}
	m, ci := installFakeSFTP(t, "posix", ops)

	_, err := m.SFTPRead(context.Background(), ci, "/tmp/file", 0, 10)
	if !errors.Is(err, ErrSFTPClientDead) || !strings.Contains(err.Error(), "force=true") || !strings.Contains(err.Error(), "MFA") {
		t.Fatalf("transport error = %v", err)
	}
	if ops.closeCount.Load() != 1 {
		t.Errorf("close count = %d, want 1", ops.closeCount.Load())
	}
	facts, _ := m.Facts(ci)
	if facts.SFTP.State != AxisDown || !strings.Contains(facts.SFTP.Reason, "lost") {
		t.Errorf("facts after loss = %+v", facts.SFTP)
	}

	_, err = m.SFTPRead(context.Background(), ci, "/tmp/file", 0, 10)
	if !errors.Is(err, ErrSFTPClientDead) {
		t.Fatalf("cached dead error = %v", err)
	}
	if len(ops.paths) != 1 {
		t.Fatalf("dead client was used again: paths=%q", ops.paths)
	}
	probe, err := m.ProbeSFTP(context.Background(), ci, false)
	if err != nil || !probe.Cached || probe.Axis.State != AxisDown {
		t.Fatalf("non-force probe after loss = %+v, err=%v", probe, err)
	}

	replacement := &fakeSFTPOperations{readData: []byte("recovered")}
	m.sftpRun = func(context.Context, *ConnInfo) (*sftpSession, SftpAxis) {
		return &sftpSession{ops: replacement}, SftpAxis{
			State: AxisUp, RealPath: "/home/mike", PathStyle: "posix", ProbedAt: time.Now(),
		}
	}
	forced, err := m.ProbeSFTP(context.Background(), ci, true)
	if err != nil || forced.Cached || forced.Axis.State != AxisUp {
		t.Fatalf("forced recovery = %+v, err=%v", forced, err)
	}
	read, err := m.SFTPRead(context.Background(), ci, "/tmp/file", 0, 20)
	if err != nil || string(read.Data) != "recovered" {
		t.Fatalf("read after forced recovery = %+v, err=%v", read, err)
	}
}

func TestSFTPProtocolErrorDoesNotRetireClient(t *testing.T) {
	ops := &fakeSFTPOperations{statErr: os.ErrNotExist}
	m, ci := installFakeSFTP(t, "posix", ops)

	_, err := m.SFTPStat(context.Background(), ci, "/missing")
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat error = %v", err)
	}
	facts, _ := m.Facts(ci)
	if facts.SFTP.State != AxisUp || ops.closeCount.Load() != 0 {
		t.Errorf("ordinary operation error retired client: facts=%+v closes=%d", facts.SFTP, ops.closeCount.Load())
	}
}

func TestSFTPCancellationRetiresClient(t *testing.T) {
	block := make(chan struct{})
	ops := &fakeSFTPOperations{readData: []byte("x"), block: block, closed: make(chan struct{})}
	m, ci := installFakeSFTP(t, "posix", ops)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := m.SFTPRead(ctx, ci, "/tmp/file", 0, 10)
	if !errors.Is(err, ErrSFTPClientDead) || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("cancellation error = %v", err)
	}
	if ops.closeCount.Load() != 1 {
		t.Errorf("close count = %d, want 1", ops.closeCount.Load())
	}
}

func TestRetiringOldSFTPGenerationDoesNotClobberReplacement(t *testing.T) {
	oldOps := &fakeSFTPOperations{}
	m, ci := installFakeSFTP(t, "posix", oldOps)
	m.sftpMu.Lock()
	old := m.sftpSessions[ci.Sock]
	newOps := &fakeSFTPOperations{}
	replacement := &sftpSession{ops: newOps}
	m.sftpSessions[ci.Sock] = replacement
	m.sftpMu.Unlock()

	m.retireSFTP(ci, old, io.ErrUnexpectedEOF)
	facts, _ := m.Facts(ci)
	if facts.SFTP.State != AxisUp {
		t.Fatalf("old generation marked replacement down: %+v", facts.SFTP)
	}
	m.sftpMu.Lock()
	got := m.sftpSessions[ci.Sock]
	m.sftpMu.Unlock()
	if got != replacement {
		t.Fatal("old generation removed replacement session")
	}
}

type fakeSFTPFile struct {
	data []byte
	mode os.FileMode
}

func atomicWriteFake(files map[string]fakeSFTPFile) *fakeSFTPOperations {
	return &fakeSFTPOperations{
		lstatFn: func(path string) (SFTPFileInfo, error) {
			file, ok := files[path]
			if !ok {
				return SFTPFileInfo{}, os.ErrNotExist
			}
			return SFTPFileInfo{Name: path, Size: int64(len(file.data)), Mode: file.mode, ModTime: time.Unix(123, 0)}, nil
		},
		writeFn: func(path string, data []byte) error {
			if _, exists := files[path]; exists {
				return os.ErrExist
			}
			files[path] = fakeSFTPFile{data: data, mode: 0o600}
			return nil
		},
		appendFn: func(path string, data []byte) error {
			file := files[path]
			file.data = append(file.data, data...)
			if file.mode == 0 {
				file.mode = 0o644
			}
			files[path] = file
			return nil
		},
		chmodFn: func(path string, mode os.FileMode) error {
			file, ok := files[path]
			if !ok {
				return os.ErrNotExist
			}
			file.mode = mode.Perm()
			files[path] = file
			return nil
		},
		removeFn: func(path string) error {
			if _, ok := files[path]; !ok {
				return os.ErrNotExist
			}
			delete(files, path)
			return nil
		},
		renameFn: func(oldPath, newPath string) error {
			file, ok := files[oldPath]
			if !ok {
				return os.ErrNotExist
			}
			files[newPath] = file
			delete(files, oldPath)
			return nil
		},
		sha256Fn: func(path string) (string, error) {
			file, ok := files[path]
			if !ok {
				return "", os.ErrNotExist
			}
			return sha256Token(file.data), nil
		},
	}
}

func TestSFTPAtomicWritePreservesModeAndVersion(t *testing.T) {
	const target = "/C:/Users/mk31/file.txt"
	files := map[string]fakeSFTPFile{target: {data: []byte("old"), mode: 0o640}}
	ops := atomicWriteFake(files)
	m, ci := installFakeSFTPWithExtensions(t, "windows", []string{"posix-rename@openssh.com"}, ops)

	result, err := m.SFTPWriteAtomic(context.Background(), ci, SFTPWriteRequest{
		Path: `C:\Users\mk31\file.txt`, Data: []byte("replacement"), IfMatch: sha256Token([]byte("old")),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Path != `C:\Users\mk31\file.txt` || result.Bytes != len("replacement") {
		t.Fatalf("result = %+v", result)
	}
	if got := files[target]; string(got.data) != "replacement" || got.mode.Perm() != 0o640 {
		t.Fatalf("installed file = %#v", got)
	}
	for name := range files {
		if strings.Contains(name, ".aishtmp.") {
			t.Fatalf("temporary file remained: %s", name)
		}
	}
}

func TestSFTPAtomicWriteRequiresOverwriteExtension(t *testing.T) {
	ops := &fakeSFTPOperations{}
	m, ci := installFakeSFTP(t, "posix", ops)
	_, err := m.SFTPWriteAtomic(context.Background(), ci, SFTPWriteRequest{Path: "/tmp/file", Data: []byte("x")})
	if !errors.Is(err, ErrSFTPAtomicReplaceUnsupported) || !strings.Contains(err.Error(), "remove-and-rename") {
		t.Fatalf("unsupported rename error = %v", err)
	}
	if len(ops.paths) != 0 {
		t.Fatalf("unsupported write touched server: %q", ops.paths)
	}
}

func TestSFTPAtomicWriteRefusesSymlinkAndStaleVersion(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    os.FileMode
		ifMatch string
		want    error
	}{
		{name: "symlink", mode: os.ModeSymlink | 0o777, want: ErrSFTPWriteSymlink},
		{name: "stale", mode: 0o600, ifMatch: sha256Token([]byte("different")), want: ErrSFTPWriteStale},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]fakeSFTPFile{"/tmp/file": {data: []byte("current"), mode: tc.mode}}
			ops := atomicWriteFake(files)
			m, ci := installFakeSFTPWithExtensions(t, "posix", []string{"posix-rename@openssh.com"}, ops)
			_, err := m.SFTPWriteAtomic(context.Background(), ci, SFTPWriteRequest{
				Path: "/tmp/file", Data: []byte("new"), IfMatch: tc.ifMatch,
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if got := string(files["/tmp/file"].data); got != "current" {
				t.Fatalf("destination changed to %q", got)
			}
			for name := range files {
				if strings.Contains(name, ".aishtmp.") {
					t.Fatalf("temporary file remained: %s", name)
				}
			}
		})
	}
}

func TestSFTPAtomicWriteMissingVersionDoesNotCreateDestination(t *testing.T) {
	files := map[string]fakeSFTPFile{}
	ops := atomicWriteFake(files)
	m, ci := installFakeSFTPWithExtensions(t, "posix", []string{"posix-rename@openssh.com"}, ops)
	_, err := m.SFTPWriteAtomic(context.Background(), ci, SFTPWriteRequest{
		Path: "/tmp/file", Data: []byte("new"), IfMatch: "mtime-size:123:3",
	})
	if !errors.Is(err, ErrSFTPWriteNoVersion) {
		t.Fatalf("error = %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("missing-version write left files: %#v", files)
	}
}

func TestSFTPAtomicWriteRefusesIgnoredMode(t *testing.T) {
	files := map[string]fakeSFTPFile{"/tmp/file": {data: []byte("current"), mode: 0o640}}
	ops := atomicWriteFake(files)
	ops.chmodFn = func(string, os.FileMode) error { return nil }
	m, ci := installFakeSFTPWithExtensions(t, "posix", []string{"posix-rename@openssh.com"}, ops)
	_, err := m.SFTPWriteAtomic(context.Background(), ci, SFTPWriteRequest{Path: "/tmp/file", Data: []byte("new")})
	if !errors.Is(err, ErrSFTPWriteMode) || !strings.Contains(err.Error(), "server reported") {
		t.Fatalf("error = %v", err)
	}
	if got := string(files["/tmp/file"].data); got != "current" {
		t.Fatalf("destination changed to %q", got)
	}
	for name := range files {
		if strings.Contains(name, ".aishtmp.") {
			t.Fatalf("temporary file remained: %s", name)
		}
	}
}

func TestSFTPAtomicWriteRechecksSymlinkBeforeRename(t *testing.T) {
	files := map[string]fakeSFTPFile{"/tmp/file": {data: []byte("current"), mode: 0o600}}
	ops := atomicWriteFake(files)
	lstat := ops.lstatFn
	calls := 0
	ops.lstatFn = func(path string) (SFTPFileInfo, error) {
		info, err := lstat(path)
		if path == "/tmp/file" {
			calls++
			if calls == 2 {
				info.Mode = os.ModeSymlink | 0o777
			}
		}
		return info, err
	}
	m, ci := installFakeSFTPWithExtensions(t, "posix", []string{"posix-rename@openssh.com"}, ops)
	_, err := m.SFTPWriteAtomic(context.Background(), ci, SFTPWriteRequest{Path: "/tmp/file", Data: []byte("new")})
	if !errors.Is(err, ErrSFTPWriteSymlink) {
		t.Fatalf("error = %v", err)
	}
	if got := string(files["/tmp/file"].data); got != "current" {
		t.Fatalf("destination changed to %q", got)
	}
}

func TestSFTPAtomicWriteTransportLossRetiresClient(t *testing.T) {
	files := map[string]fakeSFTPFile{"/tmp/file": {data: []byte("current"), mode: 0o600}}
	ops := atomicWriteFake(files)
	ops.renameFn = func(string, string) error { return io.ErrUnexpectedEOF }
	m, ci := installFakeSFTPWithExtensions(t, "posix", []string{"posix-rename@openssh.com"}, ops)
	_, err := m.SFTPWriteAtomic(context.Background(), ci, SFTPWriteRequest{Path: "/tmp/file", Data: []byte("new")})
	if !errors.Is(err, ErrSFTPClientDead) {
		t.Fatalf("transport error = %v", err)
	}
	facts, _ := m.Facts(ci)
	if facts.SFTP.State != AxisDown {
		t.Fatalf("SFTP axis = %+v", facts.SFTP)
	}
}

func TestSFTPAppendUsesNativePathAndExplicitMode(t *testing.T) {
	files := map[string]fakeSFTPFile{"/C:/Users/mk31/file.txt": {data: []byte("a"), mode: 0o600}}
	ops := atomicWriteFake(files)
	m, ci := installFakeSFTP(t, "windows", ops)
	result, err := m.SFTPAppend(context.Background(), ci, `C:\Users\mk31\file.txt`, []byte("b"), 0o640, true)
	if err != nil {
		t.Fatal(err)
	}
	if result.Path != `C:\Users\mk31\file.txt` || result.Bytes != 1 {
		t.Fatalf("result = %+v", result)
	}
	got := files["/C:/Users/mk31/file.txt"]
	if string(got.data) != "ab" || got.mode.Perm() != 0o640 {
		t.Fatalf("appended file = %#v", got)
	}
}

func TestSFTPAppendRefusesIgnoredMode(t *testing.T) {
	files := map[string]fakeSFTPFile{"/tmp/file": {data: []byte("a"), mode: 0o600}}
	ops := atomicWriteFake(files)
	ops.chmodFn = func(string, os.FileMode) error { return nil }
	m, ci := installFakeSFTP(t, "posix", ops)

	_, err := m.SFTPAppend(context.Background(), ci, "/tmp/file", []byte("b"), 0o640, true)
	if !errors.Is(err, ErrSFTPWriteMode) || !strings.Contains(err.Error(), "server reported") {
		t.Fatalf("error = %v", err)
	}
	if got := string(files["/tmp/file"].data); got != "ab" {
		t.Fatalf("append did not preserve its non-atomic contract: %q", got)
	}
}

// TestTransportLostClassifiesAmbiguousEOF pins the discrimination that keeps a
// server's SSH_FX_EOF status from being mistaken for a dead subsystem. Both
// arrive as exactly io.EOF, so only the slave's state separates them.
func TestTransportLostClassifiesAmbiguousEOF(t *testing.T) {
	exited := make(chan struct{})
	close(exited)
	alive := make(chan struct{})
	defer close(alive)

	cases := []struct {
		name    string
		session *sftpSession
		err     error
		want    bool
	}{
		{"no error", &sftpSession{processDone: alive}, nil, false},
		{"status EOF while the slave runs", &sftpSession{processDone: alive}, io.EOF, false},
		{"EOF after the slave exited", &sftpSession{processDone: exited}, io.EOF, true},
		{"EOF with no tracked slave", &sftpSession{}, io.EOF, true},
		{"unexpected EOF is always loss", &sftpSession{processDone: alive}, io.ErrUnexpectedEOF, true},
		{"closed pipe is always loss", &sftpSession{processDone: alive}, os.ErrClosed, true},
		{"broken pipe text", &sftpSession{processDone: alive}, errors.New("write |1: broken pipe"), true},
		{"missing file is not loss", &sftpSession{processDone: alive}, os.ErrNotExist, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.session.transportLost(tc.err); got != tc.want {
				t.Errorf("transportLost(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestSFTPStatusEOFWithLiveSlaveKeepsClient is the regression guard: an
// end-of-data EOF must not retire a healthy retained client, because recovering
// from that costs an explicit force and possibly another MFA prompt.
func TestSFTPStatusEOFWithLiveSlaveKeepsClient(t *testing.T) {
	ops := &fakeSFTPOperations{readErr: io.EOF, closed: make(chan struct{})}
	m := New(t.TempDir())
	ci := testConn()
	alive := make(chan struct{})
	defer close(alive)
	m.sftpRun = func(context.Context, *ConnInfo) (*sftpSession, SftpAxis) {
		return &sftpSession{ops: ops, processDone: alive}, SftpAxis{
			State: AxisUp, RealPath: "/home/mike", PathStyle: "posix", ProbedAt: time.Now(),
		}
	}
	if _, err := m.ProbeSFTP(context.Background(), ci, false); err != nil {
		t.Fatal(err)
	}

	_, err := m.SFTPRead(context.Background(), ci, "/tmp/file", 0, 10)
	if errors.Is(err, ErrSFTPClientDead) {
		t.Fatalf("a status EOF from a live slave retired the client: %v", err)
	}
	if !errors.Is(err, io.EOF) {
		t.Fatalf("read error = %v, want the underlying io.EOF", err)
	}
	facts, _ := m.Facts(ci)
	if facts.SFTP.State != AxisUp || ops.closeCount.Load() != 0 {
		t.Errorf("client retired: facts=%+v closes=%d", facts.SFTP, ops.closeCount.Load())
	}
}

// A blocked SFTP probe must fail without collateral damage. Forcing tears down
// the retained client and forgets the axis before reopening, so a refusal that
// arrived too late would destroy a working client it was never allowed to
// replace — turning a protective block into an outage.
func TestBlockedSFTPProbeLeavesRetainedClientIntact(t *testing.T) {
	ops := &fakeSFTPOperations{readData: []byte("still here")}
	m, ci := installFakeSFTPWithExtensions(t, "posix", []string{"posix-rename@openssh.com"}, ops)

	m.SetBlockNewSessions(true)
	// A cache read opens nothing, so the block must not touch it.
	cached, err := m.ProbeSFTP(context.Background(), ci, false)
	if err != nil || !cached.Cached || cached.Axis.State != AxisUp {
		t.Fatalf("blocked a probe that only read cache: %+v err=%v", cached, err)
	}
	// Forcing would close the client and reopen the subsystem, so it is refused.
	if _, err := m.ProbeSFTP(context.Background(), ci, true); !errors.Is(err, ErrNewSessionsBlocked) {
		t.Fatalf("ProbeSFTP(force=true) = %v, want ErrNewSessionsBlocked", err)
	}
	if ops.closeCount.Load() != 0 {
		t.Errorf("the retained client was closed by a refused probe (%d closes)", ops.closeCount.Load())
	}
	facts, ok := m.Facts(ci)
	if !ok || facts.SFTP.State != AxisUp {
		t.Fatalf("a refused probe damaged the cached axis: %+v", facts.SFTP)
	}

	// The whole point: file operations keep working over the client we kept.
	read, err := m.SFTPRead(context.Background(), ci, "/tmp/file", 0, 32)
	if err != nil || string(read.Data) != "still here" {
		t.Fatalf("retained client stopped serving while blocked: %+v err=%v", read, err)
	}
}
