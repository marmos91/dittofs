package blockstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/blockstore/migrate"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// captureLogger redirects the package-level logger to buf for the
// duration of t. Returns a restore func that swaps the writer back to
// io.Discard so subsequent tests don't accidentally pollute the
// terminal AND don't trip the ColorTextHandler's nil-writer panic.
func captureLogger(t *testing.T, buf *bytes.Buffer) func() {
	t.Helper()
	logger.InitWithWriter(buf, "INFO", "json", false)
	return func() {
		logger.InitWithWriter(io.Discard, "INFO", "text", false)
	}
}

// stubFile builds a walkedFile with the given handle and an empty
// metadata.File. Tests don't actually re-chunk, so the File payload
// is irrelevant.
func stubFile(h string) walkedFile {
	return walkedFile{
		Handle: metadata.FileHandle(h),
		Attr:   &metadata.File{},
	}
}

// TestWorkerPool_Concurrency (Test 1): with parallel=4 and 8 files,
// the maximum observed in-flight worker count reaches 4.
func TestWorkerPool_Concurrency(t *testing.T) {
	const numFiles = 8
	const parallel = 4

	files := make([]walkedFile, numFiles)
	for i := range files {
		files[i] = stubFile("h-" + string(rune('a'+i)))
	}

	var inflight, maxInflight atomic.Int32
	hold := make(chan struct{})

	prev := workerPoolMigrateOneFile
	defer func() { workerPoolMigrateOneFile = prev }()
	workerPoolMigrateOneFile = func(
		_ context.Context,
		_ *offlineRuntime,
		_ *migrate.Journal,
		_ migrateOptions,
		_ *rate.Limiter,
		_ metadata.FileHandle,
		_ *metadata.File,
	) (perFileResult, error) {
		cur := inflight.Add(1)
		for {
			old := maxInflight.Load()
			if cur <= old || maxInflight.CompareAndSwap(old, cur) {
				break
			}
		}
		<-hold
		inflight.Add(-1)
		return perFileResult{}, nil
	}

	pool := newWorkerPool(parallel, nil, nil, migrateOptions{}, nil, nil)

	done := make(chan error, 1)
	go func() {
		_, err := pool.Run(context.Background(), files)
		done <- err
	}()

	deadline := time.After(5 * time.Second)
	for {
		if maxInflight.Load() >= int32(parallel) {
			break
		}
		select {
		case <-deadline:
			close(hold)
			t.Fatalf("max in-flight workers = %d after 5s; want %d", maxInflight.Load(), parallel)
		case <-time.After(10 * time.Millisecond):
		}
	}
	close(hold)

	if err := <-done; err != nil {
		t.Fatalf("pool.Run err=%v", err)
	}
	if got := maxInflight.Load(); got != int32(parallel) {
		t.Errorf("maxInflight = %d, want exactly %d", got, parallel)
	}
}

// TestWorkerPool_Serial (Test 2): with parallel=1, observed concurrency
// never exceeds 1.
func TestWorkerPool_Serial(t *testing.T) {
	const numFiles = 4
	files := make([]walkedFile, numFiles)
	for i := range files {
		files[i] = stubFile(string(rune('a' + i)))
	}

	var inflight, maxInflight atomic.Int32

	prev := workerPoolMigrateOneFile
	defer func() { workerPoolMigrateOneFile = prev }()
	workerPoolMigrateOneFile = func(
		_ context.Context,
		_ *offlineRuntime,
		_ *migrate.Journal,
		_ migrateOptions,
		_ *rate.Limiter,
		_ metadata.FileHandle,
		_ *metadata.File,
	) (perFileResult, error) {
		cur := inflight.Add(1)
		defer inflight.Add(-1)
		for {
			old := maxInflight.Load()
			if cur <= old || maxInflight.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		return perFileResult{}, nil
	}

	pool := newWorkerPool(1, nil, nil, migrateOptions{}, nil, nil)
	if _, err := pool.Run(context.Background(), files); err != nil {
		t.Fatalf("pool.Run err=%v", err)
	}
	if got := maxInflight.Load(); got > 1 {
		t.Errorf("maxInflight = %d under parallel=1; want 1", got)
	}
}

// TestWorkerPool_FirstErrorCancels (Test 3): the first worker error
// cancels the errgroup ctx; remaining files are never started.
func TestWorkerPool_FirstErrorCancels(t *testing.T) {
	const numFiles = 32
	files := make([]walkedFile, numFiles)
	for i := range files {
		files[i] = stubFile("h-" + strings.Repeat("x", i+1))
	}

	var started atomic.Int32
	wantErr := errors.New("simulated worker failure")

	prev := workerPoolMigrateOneFile
	defer func() { workerPoolMigrateOneFile = prev }()
	workerPoolMigrateOneFile = func(
		ctx context.Context,
		_ *offlineRuntime,
		_ *migrate.Journal,
		_ migrateOptions,
		_ *rate.Limiter,
		h metadata.FileHandle,
		_ *metadata.File,
	) (perFileResult, error) {
		started.Add(1)
		if string(h) == "h-x" {
			return perFileResult{}, wantErr
		}
		select {
		case <-ctx.Done():
			return perFileResult{}, ctx.Err()
		case <-time.After(2 * time.Second):
			return perFileResult{}, nil
		}
	}

	pool := newWorkerPool(2, nil, nil, migrateOptions{}, nil, nil)
	_, err := pool.Run(context.Background(), files)
	if err == nil {
		t.Fatal("pool.Run err=nil; want non-nil")
	}
	if !errors.Is(err, wantErr) && !strings.Contains(err.Error(), "simulated worker failure") {
		t.Errorf("pool.Run err=%v; want simulated worker failure", err)
	}
	if got := started.Load(); got >= int32(numFiles) {
		t.Errorf("all %d workers started despite cancellation (started=%d)", numFiles, got)
	}
}

// TestWorkerPool_PreservesIsFileDoneSkip (Test 4): files already
// recorded as done in the journal are not dispatched to workers; their
// FilesSkipped count increments instead.
func TestWorkerPool_PreservesIsFileDoneSkip(t *testing.T) {
	tmp := t.TempDir()
	journal, err := migrate.OpenJournal(tmp)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	defer journal.Close()
	doneHandle := metadata.FileHandle("done-h")
	if err := journal.Append(migrate.JournalEntry{
		Kind:       "file_done",
		FileHandle: string(doneHandle),
	}); err != nil {
		t.Fatalf("seed Append: %v", err)
	}

	files := []walkedFile{
		{Handle: doneHandle, Attr: &metadata.File{}},
		stubFile("fresh-1"),
		stubFile("fresh-2"),
	}

	var dispatched atomic.Int32
	var dispatchedHandles sync.Map

	prev := workerPoolMigrateOneFile
	defer func() { workerPoolMigrateOneFile = prev }()
	workerPoolMigrateOneFile = func(
		_ context.Context,
		_ *offlineRuntime,
		_ *migrate.Journal,
		_ migrateOptions,
		_ *rate.Limiter,
		h metadata.FileHandle,
		_ *metadata.File,
	) (perFileResult, error) {
		dispatched.Add(1)
		dispatchedHandles.Store(string(h), true)
		return perFileResult{}, nil
	}

	pool := newWorkerPool(2, nil, journal, migrateOptions{}, nil, nil)
	res, err := pool.Run(context.Background(), files)
	if err != nil {
		t.Fatalf("pool.Run err=%v", err)
	}
	if got := dispatched.Load(); got != 2 {
		t.Errorf("dispatched = %d; want 2 (seeded handle must be skipped)", got)
	}
	if _, ok := dispatchedHandles.Load(string(doneHandle)); ok {
		t.Errorf("seeded done handle was re-dispatched")
	}
	if res.FilesSkipped != 1 {
		t.Errorf("FilesSkipped = %d; want 1", res.FilesSkipped)
	}
	if res.FilesDone != 2 {
		t.Errorf("FilesDone = %d; want 2", res.FilesDone)
	}
}

// TestProgressReporter_SlogEvent (Test 5): every per-file commit
// emits an "migrate.file.committed" event with the expected fields.
func TestProgressReporter_SlogEvent(t *testing.T) {
	var buf bytes.Buffer
	restore := captureLogger(t, &buf)
	defer restore()

	p := newProgressReporter(2)
	defer p.Close()
	// Force TTY off to keep stdout clean during the test.
	p.ttyEnabled = false

	p.OnFileCommit(perFileResult{
		BytesUploaded: 1234,
		BytesDeduped:  567,
	})

	out := buf.String()
	if !strings.Contains(out, "migrate.file.committed") {
		t.Errorf("logger output missing event name; got:\n%s", out)
	}
	for _, k := range []string{"bytes_uploaded", "bytes_deduped", "files_done", "files_total"} {
		if !strings.Contains(out, k) {
			t.Errorf("logger output missing key %q; got:\n%s", k, out)
		}
	}
}

// TestProgressReporter_TTYBarOverlays (Test 6, TTY case): when
// ttyEnabled=true, the progress reporter writes a \r-prefixed line to
// the configured writer.
func TestProgressReporter_TTYBarOverlays(t *testing.T) {
	var buf bytes.Buffer
	p := &progressReporter{
		ttyEnabled: true,
		total:      4,
		startedAt:  time.Now(),
		out:        &buf,
	}
	defer p.Close()

	p.OnFileCommit(perFileResult{BytesUploaded: 1024})
	out := buf.String()
	if !strings.Contains(out, "\r") {
		t.Errorf("TTY progress bar missing \\r; got %q", out)
	}
	if !strings.Contains(out, "1/4") {
		t.Errorf("TTY progress bar missing 1/4 progress; got %q", out)
	}
}

// TestProgressReporter_NoTTYBarOnPipe (Test 6, non-TTY): when
// ttyEnabled=false, no \r-prefixed lines are written.
func TestProgressReporter_NoTTYBarOnPipe(t *testing.T) {
	var buf bytes.Buffer
	p := &progressReporter{
		ttyEnabled: false,
		total:      4,
		startedAt:  time.Now(),
		out:        &buf,
	}
	defer p.Close()
	p.OnFileCommit(perFileResult{BytesUploaded: 1024})
	if strings.Contains(buf.String(), "\r") {
		t.Errorf("non-TTY mode wrote \\r-overlaid line; got %q", buf.String())
	}
}
