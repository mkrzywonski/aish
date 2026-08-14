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
	t.Helper()
	m := New(t.TempDir())
	ci := testConn()
	m.sftpRun = func(context.Context, *ConnInfo) (*sftpSession, SftpAxis) {
		return &sftpSession{ops: ops}, SftpAxis{
			State: AxisUp, RealPath: "/home/mike", PathStyle: style, ProbedAt: time.Now(),
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
