package fs

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	memmeta "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// loggerCaptureMu serializes log-output redirection across tests in this
// file so two parallel-aware tests can't trample each other's writers.
// The package's tests do not call t.Parallel() today, so this is purely
// defensive — but the global logger output is process-wide, so the
// discipline matters.
var loggerCaptureMu sync.Mutex

// safeBuffer wraps bytes.Buffer with a mutex so the logger goroutine and
// the assertion goroutine cannot race on the underlying slice. Required
// because slog handlers may call Write from any goroutine.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLogger swaps the global logger output to an in-memory buffer for
// the duration of the calling test, restoring stdout afterward via
// t.Cleanup. Returns the buffer for assertion. The caller MUST NOT call
// t.Parallel — the global logger is process-wide.
func captureLogger(t *testing.T) *safeBuffer {
	t.Helper()
	loggerCaptureMu.Lock()
	t.Cleanup(loggerCaptureMu.Unlock)
	buf := &safeBuffer{}
	logger.InitWithWriter(buf, "WARN", "text", false)
	t.Cleanup(func() {
		// Restore default stdout JSON-or-text writer at INFO. Tests do
		// not require pristine logger state across test boundaries —
		// the package-level loggerCaptureMu provides that — but
		// pointing the writer at stdout again means subsequent test
		// failures show up in `go test -v` output as expected.
		logger.InitWithWriter(os.Stdout, "INFO", "text", false)
	})
	return buf
}

// reopenFSStore closes bc, then constructs a new FSStore on the same baseDir
// with the same RollupStore, triggering Recover.
func reopenFSStore(t *testing.T, bc *FSStore, rs *memmeta.MemoryMetadataStore) *FSStore {
	t.Helper()
	baseDir := bc.baseDir
	_ = bc.Close()
	bc2, err := NewWithOptions(baseDir, 1<<30, 1<<30, nopFBS{}, FSStoreOptions{
		UseAppendLog:    true,
		MaxLogBytes:     1 << 30,
		RollupWorkers:   2,
		StabilizationMS: 10,
		RollupStore:     rs,
	})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := bc2.Recover(context.Background()); err != nil {
		t.Fatalf("reopen Recover: %v", err)
	}
	t.Cleanup(func() { _ = bc2.Close() })
	return bc2
}

func newFSStoreWithRS(t *testing.T, rs *memmeta.MemoryMetadataStore) *FSStore {
	t.Helper()
	dir := t.TempDir()
	bc, err := NewWithOptions(dir, 1<<30, 1<<30, nopFBS{}, FSStoreOptions{
		UseAppendLog:    true,
		MaxLogBytes:     1 << 30,
		RollupWorkers:   2,
		StabilizationMS: 10,
		RollupStore:     rs,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	t.Cleanup(func() { _ = bc.Close() })
	return bc
}

func TestRecovery_HappyPath_NoOp(t *testing.T) {
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	bc := newFSStoreWithRS(t, rs)
	if err := bc.AppendWrite(context.Background(), "file1", []byte("hello"), 0); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}
	bc2 := reopenFSStore(t, bc, rs)
	bc2.logsMu.RLock()
	tree := bc2.dirtyIntervals["file1"]
	bc2.logsMu.RUnlock()
	if tree == nil || tree.Len() == 0 {
		t.Fatalf("expected 1 interval rebuilt, got %v", tree)
	}
}

func TestRecovery_TornRecord_TruncatesAtFirstBadCRC(t *testing.T) {
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	bc := newFSStoreWithRS(t, rs)
	payload := bytes.Repeat([]byte{0xAA}, 100)
	if err := bc.AppendWrite(context.Background(), "file1", payload, 0); err != nil {
		t.Fatalf("AppendWrite 1: %v", err)
	}
	if err := bc.AppendWrite(context.Background(), "file1", payload, 4096); err != nil {
		t.Fatalf("AppendWrite 2: %v", err)
	}
	if err := bc.AppendWrite(context.Background(), "file1", payload, 8192); err != nil {
		t.Fatalf("AppendWrite 3: %v", err)
	}
	baseDir := bc.baseDir
	// Close, then append garbage.
	_ = bc.Close()
	path := filepath.Join(baseDir, "logs", "file1.log")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		t.Fatalf("open for garbage append: %v", err)
	}
	if _, err := f.Write([]byte("garbage-not-a-record")); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	_ = f.Close()

	// reopenFSStore uses bc.baseDir — already fetched above, but reopen
	// helper reads baseDir field (still readable after Close).
	bc2 := reopenFSStore(t, bc, rs)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	want := int64(logHeaderSize + 3*(recordFrameOverhead+len(payload)))
	if st.Size() != want {
		t.Fatalf("truncated size: got %d want %d", st.Size(), want)
	}
	bc2.logsMu.RLock()
	tree := bc2.dirtyIntervals["file1"]
	bc2.logsMu.RUnlock()
	if tree == nil || tree.Len() != 3 {
		t.Fatalf("interval tree: len=%d want 3", func() int {
			if tree == nil {
				return -1
			}
			return tree.Len()
		}())
	}
}

func TestRecovery_HeaderReconcile_AfterMetadataAdvance(t *testing.T) {
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	bc := newFSStoreWithRS(t, rs)
	payload := bytes.Repeat([]byte{0xBB}, 200)
	if err := bc.AppendWrite(context.Background(), "file1", payload, 0); err != nil {
		t.Fatalf("AppendWrite 1: %v", err)
	}
	if err := bc.AppendWrite(context.Background(), "file1", payload, 4096); err != nil {
		t.Fatalf("AppendWrite 2: %v", err)
	}
	_ = bc.Close()
	// Simulate the crash window: metadata advanced, header did not.
	targetOff := uint64(logHeaderSize + recordFrameOverhead + len(payload))
	if _, err := rs.SetRollupOffset(context.Background(), "file1", targetOff); err != nil {
		t.Fatalf("pre-seed SetRollupOffset: %v", err)
	}

	bc2 := reopenFSStore(t, bc, rs)
	path := filepath.Join(bc2.baseDir, "logs", "file1.log")
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()
	hdr, err := readLogHeader(f)
	if err != nil {
		t.Fatalf("readLogHeader: %v", err)
	}
	if hdr.RollupOffset != targetOff {
		t.Fatalf("header not reconciled: got %d want %d", hdr.RollupOffset, targetOff)
	}
}

func TestRecovery_BadHeaderMagic_Reinit(t *testing.T) {
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	bc := newFSStoreWithRS(t, rs)
	if err := bc.AppendWrite(context.Background(), "file1", []byte("x"), 0); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}
	baseDir := bc.baseDir
	_ = bc.Close()
	path := filepath.Join(baseDir, "logs", "file1.log")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	raw[0] = 'X' // corrupt magic
	if err := os.WriteFile(path, raw, 0644); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}

	_ = reopenFSStore(t, bc, rs)
	// File should have been re-initialized: size == logHeaderSize, magic valid.
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open after re-init: %v", err)
	}
	defer func() { _ = f.Close() }()
	hdr, err := readLogHeader(f)
	if err != nil {
		t.Fatalf("after re-init: readLogHeader err=%v", err)
	}
	if hdr.Magic != logMagic {
		t.Fatalf("magic still corrupt: %v", hdr.Magic)
	}
}

// TestRecovery_FreshLogNotSwept asserts Warning 3 fix: a log whose mtime is
// within orphanLogMinAge (default 3600s) is NOT swept even if metadata is
// absent and FileBlockStore has no block-0. Fresh logs that have not yet
// been rolled up must survive boot.
func TestRecovery_FreshLogNotSwept(t *testing.T) {
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	bc := newFSStoreWithRS(t, rs)
	if err := bc.AppendWrite(context.Background(), "fresh-ghost", []byte("x"), 0); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}
	baseDir := bc.baseDir
	_ = bc.Close()
	// Do NOT chtimes — mtime is "now" so the age gate rejects sweep.

	_ = reopenFSStore(t, bc, rs)
	path := filepath.Join(baseDir, "logs", "fresh-ghost.log")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("fresh log was swept (should have been preserved): err=%v", err)
	}
}

// TestRecovery_OrphanSweep_UnlinksLog asserts the full orphan-sweep path
// with a custom (short) OrphanLogMinAgeSeconds so we can exercise the age
// gate without forging mtimes via os.Chtimes. This complements
// TestRecovery_OldOrphanLogSwept (which uses the default 3600s gate and
// chtimes) by verifying the OrphanLogMinAgeSeconds option wiring.
func TestRecovery_OrphanSweep_UnlinksLog(t *testing.T) {
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	dir := t.TempDir()
	bc, err := NewWithOptions(dir, 1<<30, 1<<30, nopFBS{}, FSStoreOptions{
		UseAppendLog:           true,
		MaxLogBytes:            1 << 30,
		RollupWorkers:          2,
		StabilizationMS:        10,
		RollupStore:            rs,
		OrphanLogMinAgeSeconds: 1, // 1 second — short enough to trip during the test
	})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	if err := bc.AppendWrite(context.Background(), "ghost", []byte("x"), 0); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}
	_ = bc.Close()
	path := filepath.Join(dir, "logs", "ghost.log")
	// Wait out the configured 1s age gate.
	time.Sleep(1100 * time.Millisecond)

	bc2, err := NewWithOptions(dir, 1<<30, 1<<30, nopFBS{}, FSStoreOptions{
		UseAppendLog:           true,
		MaxLogBytes:            1 << 30,
		RollupWorkers:          2,
		StabilizationMS:        10,
		RollupStore:            rs,
		OrphanLogMinAgeSeconds: 1,
	})
	if err != nil {
		t.Fatalf("reopen NewWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = bc2.Close() })
	if err := bc2.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("ghost log not swept: err=%v", err)
	}
}

// TestRecovery_OldOrphanLogSwept explicitly exercises the age-gate path.
func TestRecovery_OldOrphanLogSwept(t *testing.T) {
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	bc := newFSStoreWithRS(t, rs)
	if err := bc.AppendWrite(context.Background(), "old-ghost", []byte("x"), 0); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}
	baseDir := bc.baseDir
	_ = bc.Close()
	path := filepath.Join(baseDir, "logs", "old-ghost.log")
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	_ = reopenFSStore(t, bc, rs)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("old orphan log not swept: err=%v", err)
	}
}

// TestRecovery_RepeatedCorruption_OrphanAgePreserved asserts FIX-13: a log
// whose header is corrupted and re-initialized during recovery must keep
// its pre-boot mtime, otherwise the orphan-sweep age clock resets every
// boot and a repeatedly-corrupted log can never be considered "old enough"
// to sweep.
func TestRecovery_RepeatedCorruption_OrphanAgePreserved(t *testing.T) {
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	bc := newFSStoreWithRS(t, rs)
	if err := bc.AppendWrite(context.Background(), "corrupt-old", []byte("x"), 0); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}
	baseDir := bc.baseDir
	_ = bc.Close()
	path := filepath.Join(baseDir, "logs", "corrupt-old.log")

	// Age the log far past the default 3600s gate.
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	// Corrupt the header magic so recovery takes the reinit path.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	raw[0] = 'X'
	if err := os.WriteFile(path, raw, 0644); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}
	// WriteFile bumps mtime — restore the aged mtime so we test the
	// reinit-preserves-mtime invariant rather than the WriteFile bump.
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("chtimes after corrupt: %v", err)
	}

	_ = reopenFSStore(t, bc, rs)

	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after reinit: %v", err)
	}
	// The reinit must NOT have reset the mtime to "now". Allow 5 minutes of
	// drift to absorb filesystem mtime granularity differences across CI.
	if delta := time.Since(st.ModTime()); delta < time.Hour {
		t.Fatalf("reinit reset mtime: file appears %s old, want >=1h (orphan-age clock would reset)",
			delta)
	}
}

// TestRecovery_RejectsLongPayloadIDFromDisk asserts the LENGTH boundary of
// FIX-18's payloadID validator: a filename whose stem exceeds 128
// alphanumeric characters MUST be skipped at the WalkDir boundary even
// though the character class itself is conforming. This guards the upper
// bound of the {1,128} quantifier in payloadIDPattern.
func TestRecovery_RejectsLongPayloadIDFromDisk(t *testing.T) {
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	bc := newFSStoreWithRS(t, rs)

	// Seed at least one valid log so recovery walks the dir.
	if err := bc.AppendWrite(context.Background(), "valid", []byte("x"), 0); err != nil {
		t.Fatalf("AppendWrite valid: %v", err)
	}

	// 129-char alphanumeric stem — one past the validator ceiling.
	stem := strings.Repeat("a", 129)
	logsDir := filepath.Join(bc.baseDir, "logs")
	badPath := filepath.Join(logsDir, stem+".log")
	if err := os.WriteFile(badPath, []byte("garbage"), 0644); err != nil {
		t.Fatalf("seed long-name file: %v", err)
	}

	bc2 := reopenFSStore(t, bc, rs)

	// The over-long file must still exist (recovery never touched it).
	if _, err := os.Stat(badPath); err != nil {
		t.Fatalf("long-payloadID file was unexpectedly removed by recovery: %v", err)
	}

	// No FSStore state must have been created under the over-long payloadID.
	bc2.logsMu.RLock()
	defer bc2.logsMu.RUnlock()
	if _, ok := bc2.logFDs[stem]; ok {
		t.Fatalf("FSStore created logFDs entry for over-long payloadID")
	}
	if _, ok := bc2.dirtyIntervals[stem]; ok {
		t.Fatalf("FSStore created dirtyIntervals entry for over-long payloadID")
	}
}

// TestRecovery_RejectsInvalidPayloadIDFromDisk asserts FIX-18: a file
// dropped into the logs directory with a path-traversing or otherwise
// non-conformant name MUST be skipped by recovery — never opened, never
// truncated, never unlinked. The file remains on disk after recovery
// runs and no FSStore state is created for it.
func TestRecovery_RejectsInvalidPayloadIDFromDisk(t *testing.T) {
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	bc := newFSStoreWithRS(t, rs)

	// Seed at least one valid log so recovery actually walks the dir.
	if err := bc.AppendWrite(context.Background(), "valid", []byte("x"), 0); err != nil {
		t.Fatalf("AppendWrite valid: %v", err)
	}

	// Drop a file with a malicious-looking payloadID directly into the
	// logs dir. Use only filename-safe chars so os.WriteFile succeeds; the
	// validation rule rejects characters outside [a-zA-Z0-9_-].
	logsDir := filepath.Join(bc.baseDir, "logs")
	badName := "..bad..payload.id.log"
	badPath := filepath.Join(logsDir, badName)
	if err := os.WriteFile(badPath, []byte("garbage"), 0644); err != nil {
		t.Fatalf("seed bad file: %v", err)
	}

	bc2 := reopenFSStore(t, bc, rs)

	// The bad file must still exist (recovery never touched it).
	if _, err := os.Stat(badPath); err != nil {
		t.Fatalf("bad-payloadID file was unexpectedly removed by recovery: %v", err)
	}

	// No FSStore state must have been created under the bad payloadID.
	bc2.logsMu.RLock()
	defer bc2.logsMu.RUnlock()
	for k := range bc2.logFDs {
		if strings.Contains(k, "..") {
			t.Fatalf("FSStore created state for invalid payloadID %q", k)
		}
	}
	for k := range bc2.dirtyIntervals {
		if strings.Contains(k, "..") {
			t.Fatalf("FSStore created interval-tree for invalid payloadID %q", k)
		}
	}
}

// TestRecovery_OrphanAgeFloor_WarnsOnNonPositive asserts FIX-16: when the
// effective orphanLogMinAgeSeconds is non-positive (a misconfiguration
// that bypasses NewWithOptions' default-injection — e.g., a future code
// path that sets it post-construction or a struct-literal caller), the
// recovery pass MUST log a Warn at boot AND substitute the safe 3600s
// floor for the actual sweep gate. This keeps a misconfigured operator
// from silently sweeping fresh logs at boot.
//
// The effective minAge substitution is verified indirectly by writing a
// log with a recent mtime and asserting it survives recovery despite
// minAge==0 (a literal 0 would have classified the log as instantly
// sweepable).
func TestRecovery_OrphanAgeFloor_WarnsOnNonPositive(t *testing.T) {
	buf := captureLogger(t)

	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	dir := t.TempDir()
	bc, err := NewWithOptions(dir, 1<<30, 1<<30, nopFBS{}, FSStoreOptions{
		UseAppendLog:           true,
		MaxLogBytes:            1 << 30,
		RollupWorkers:          2,
		StabilizationMS:        10,
		RollupStore:            rs,
		OrphanLogMinAgeSeconds: 1, // pass NewWithOptions normalization
	})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	// Bypass normalization: flip the field to 0 directly. This models a
	// post-construction misconfiguration the FIX-16 Warn was added to
	// catch.
	bc.orphanLogMinAgeSeconds = 0

	// Write a log so recovery has something to walk.
	if werr := bc.AppendWrite(context.Background(), "fresh", []byte("x"), 0); werr != nil {
		t.Fatalf("AppendWrite: %v", werr)
	}
	logPath := filepath.Join(dir, "logs", "fresh.log")
	if _, serr := os.Stat(logPath); serr != nil {
		t.Fatalf("log not created: %v", serr)
	}
	_ = bc.Close()

	bc2, err := NewWithOptions(dir, 1<<30, 1<<30, nopFBS{}, FSStoreOptions{
		UseAppendLog:           true,
		MaxLogBytes:            1 << 30,
		RollupWorkers:          2,
		StabilizationMS:        10,
		RollupStore:            rs,
		OrphanLogMinAgeSeconds: 1, // again pass normalization
	})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = bc2.Close() })
	// Force the misconfiguration into the new instance too so recovery
	// hits the FIX-16 substitution branch.
	bc2.orphanLogMinAgeSeconds = 0
	if rerr := bc2.Recover(context.Background()); rerr != nil {
		t.Fatalf("Recover: %v", rerr)
	}

	// Assertion 1: the FIX-16 Warn fired.
	logged := buf.String()
	if !strings.Contains(logged, "orphan_log_min_age_seconds is non-positive") {
		t.Fatalf("FIX-16 Warn not emitted; captured log:\n%s", logged)
	}

	// Assertion 2: the effective floor was substituted (3600s). The
	// recently-written log MUST still exist — a literal 0 floor would
	// have swept it. Survival of the freshly-written log is the
	// behavioral proof that the substitution took effect.
	if _, serr := os.Stat(logPath); serr != nil {
		t.Fatalf("fresh log was swept despite FIX-16 floor substitution: %v", serr)
	}
}
