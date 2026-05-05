package blockstore

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/remote"
	"github.com/marmos91/dittofs/pkg/blockstore/remote/memory"
	"github.com/marmos91/dittofs/pkg/health"
)

// stubRemoteStore wraps a memory.Store but lets each test override
// HeadObject's behavior to inject 404s, header mismatches, transient
// errors, and to observe concurrency.
type stubRemoteStore struct {
	*memory.Store

	headFn func(ctx context.Context, key string) (remote.HeadResult, error)

	mu          sync.Mutex
	inFlight    int
	maxInFlight int
}

func newStubRemoteStore() *stubRemoteStore {
	return &stubRemoteStore{Store: memory.New()}
}

func (s *stubRemoteStore) HeadObject(ctx context.Context, key string) (remote.HeadResult, error) {
	s.mu.Lock()
	s.inFlight++
	if s.inFlight > s.maxInFlight {
		s.maxInFlight = s.inFlight
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inFlight--
		s.mu.Unlock()
	}()

	if s.headFn != nil {
		return s.headFn(ctx, key)
	}
	return s.Store.HeadObject(ctx, key)
}

// MaxInFlight returns the peak observed concurrent HEAD count.
func (s *stubRemoteStore) MaxInFlight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxInFlight
}

var _ remote.RemoteStore = (*stubRemoteStore)(nil)

func (s *stubRemoteStore) Healthcheck(ctx context.Context) health.Report {
	return s.Store.Healthcheck(ctx)
}

// integrityFixture wires a memory metadata store + a stubRemoteStore +
// the offlineRuntime composite so verifyIntegrity tests can drive the
// full code path via the real WalkShareFiles helper.
type integrityFixture struct {
	*loopFixture
	stub *stubRemoteStore
}

// newIntegrityFixture is loopFixture but with a stubRemoteStore instead
// of the bare memory.Store, so HEAD behavior is injectable per test.
func newIntegrityFixture(t *testing.T) *integrityFixture {
	t.Helper()
	base := newLoopFixture(t)
	stub := newStubRemoteStore()
	base.rt = newTestOfflineRuntime(base.share, base.mds, base.mds, stub, base.dataDir)
	base.rs = stub.Store
	return &integrityFixture{loopFixture: base, stub: stub}
}

// runFullMigration runs the migrate loop end-to-end so post-migration
// FileAttr.Blocks are populated against the stub remote (CAS objects
// land in stub.Store). Returns the number of unique hashes the loop
// produced (== len(uniq across files)).
func runFullMigration(t *testing.T, f *integrityFixture) int {
	t.Helper()
	opts := migrateOptions{share: f.share, stateDir: f.dataDir}
	if err := runMigrateLoopWithRuntime(t.Context(), f.rt, opts); err != nil {
		t.Fatalf("runMigrateLoopWithRuntime: %v", err)
	}
	uniq := map[blockstore.ContentHash]struct{}{}
	keys, _ := f.stub.ListByPrefix(t.Context(), "cas/")
	for _, k := range keys {
		h, err := blockstore.ParseCASKey(k)
		if err == nil {
			uniq[h] = struct{}{}
		}
	}
	return len(uniq)
}

// TestVerifyIntegrity_EmptyShare covers behavior V1: empty share →
// nil error + zero HEAD calls + zero unique hashes.
func TestVerifyIntegrity_EmptyShare(t *testing.T) {
	f := newIntegrityFixture(t)
	res, err := verifyIntegrity(t.Context(), f.rt, migrateOptions{share: f.share})
	if err != nil {
		t.Fatalf("verifyIntegrity: %v", err)
	}
	if res.UniqueHashes != 0 {
		t.Errorf("UniqueHashes = %d, want 0", res.UniqueHashes)
	}
	if res.HEADCalls != 0 {
		t.Errorf("HEADCalls = %d, want 0", res.HEADCalls)
	}
}

// TestVerifyIntegrity_HappyPath covers behavior V2 partly: after a real
// migration loop, verifyIntegrity issues exactly one HEAD per unique
// hash and returns nil.
func TestVerifyIntegrity_HappyPath(t *testing.T) {
	f := newIntegrityFixture(t)
	addLegacyFile(t, f.loopFixture, "a.bin", "/a.bin", [][]byte{largeChunk('a')})
	addLegacyFile(t, f.loopFixture, "b.bin", "/b.bin", [][]byte{largeChunk('b')})
	addLegacyFile(t, f.loopFixture, "c.bin", "/c.bin", [][]byte{largeChunk('c')})
	uniqExpected := runFullMigration(t, f)

	res, err := verifyIntegrity(t.Context(), f.rt, migrateOptions{share: f.share})
	if err != nil {
		t.Fatalf("verifyIntegrity: %v", err)
	}
	if res.UniqueHashes != uniqExpected {
		t.Errorf("UniqueHashes = %d, want %d", res.UniqueHashes, uniqExpected)
	}
	if res.HEADCalls != uniqExpected {
		t.Errorf("HEADCalls = %d, want %d (linear in unique hashes per D-A12)",
			res.HEADCalls, uniqExpected)
	}
	if len(res.Failures) != 0 {
		t.Errorf("Failures = %v, want []", res.Failures)
	}
}

// TestVerifyIntegrity_DedupHitsCountOncePerHash covers behavior V2 (full):
// 3 files all with identical content → 1 unique hash → exactly 1 HEAD.
func TestVerifyIntegrity_DedupHitsCountOncePerHash(t *testing.T) {
	f := newIntegrityFixture(t)
	dup := largeChunk('z')
	addLegacyFile(t, f.loopFixture, "a.bin", "/a.bin", [][]byte{dup})
	addLegacyFile(t, f.loopFixture, "b.bin", "/b.bin", [][]byte{dup})
	addLegacyFile(t, f.loopFixture, "c.bin", "/c.bin", [][]byte{dup})
	uniqExpected := runFullMigration(t, f)
	if uniqExpected == 0 {
		t.Fatal("setup produced zero unique hashes")
	}

	res, err := verifyIntegrity(t.Context(), f.rt, migrateOptions{share: f.share})
	if err != nil {
		t.Fatalf("verifyIntegrity: %v", err)
	}
	// All three files share the SAME chunk hashes; the integrity walk
	// dedups via the union map, so HEADCalls equals the unique count.
	if res.HEADCalls != uniqExpected {
		t.Errorf("HEADCalls = %d, want %d (dedup union)", res.HEADCalls, uniqExpected)
	}
}

// TestVerifyIntegrity_MissingKey covers behavior V3: HEAD returns 404
// for one hash → wrapped ErrIntegrityCheckFailed with the missing key
// in the error message.
func TestVerifyIntegrity_MissingKey(t *testing.T) {
	f := newIntegrityFixture(t)
	addLegacyFile(t, f.loopFixture, "a.bin", "/a.bin", [][]byte{largeChunk('a')})
	runFullMigration(t, f)

	// Inject a 404 for every HEAD so we are guaranteed to surface a
	// failure. (We could also pre-stub a single key; this is simpler
	// and asserts the failure-path wrapping.)
	f.stub.headFn = func(_ context.Context, key string) (remote.HeadResult, error) {
		return remote.HeadResult{}, blockstore.ErrBlockNotFound
	}

	res, err := verifyIntegrity(t.Context(), f.rt, migrateOptions{share: f.share})
	if err == nil {
		t.Fatal("verifyIntegrity: expected ErrIntegrityCheckFailed, got nil")
	}
	if !errors.Is(err, ErrIntegrityCheckFailed) {
		t.Fatalf("verifyIntegrity err = %v, want wrapped ErrIntegrityCheckFailed", err)
	}
	if len(res.Failures) == 0 {
		t.Fatal("res.Failures is empty; expected >=1 entry")
	}
	if !strings.Contains(res.Failures[0], "missing") {
		t.Errorf("res.Failures[0] = %q, want substring \"missing\"", res.Failures[0])
	}
}

// TestVerifyIntegrity_HeaderMismatch covers behavior V4: HEAD returns
// 200 but the content-hash header does not equal blake3:{hex(key)} →
// ErrIntegrityCheckFailed with header-mismatch detail.
func TestVerifyIntegrity_HeaderMismatch(t *testing.T) {
	f := newIntegrityFixture(t)
	addLegacyFile(t, f.loopFixture, "a.bin", "/a.bin", [][]byte{largeChunk('a')})
	runFullMigration(t, f)

	// Inject HEADs that report a wrong header value.
	f.stub.headFn = func(_ context.Context, key string) (remote.HeadResult, error) {
		return remote.HeadResult{
			ContentLength: 1,
			Metadata:      map[string]string{"content-hash": "blake3:WRONG"},
		}, nil
	}

	res, err := verifyIntegrity(t.Context(), f.rt, migrateOptions{share: f.share})
	if err == nil {
		t.Fatal("verifyIntegrity: expected ErrIntegrityCheckFailed, got nil")
	}
	if !errors.Is(err, ErrIntegrityCheckFailed) {
		t.Fatalf("verifyIntegrity err = %v, want wrapped ErrIntegrityCheckFailed", err)
	}
	if len(res.Failures) == 0 {
		t.Fatal("res.Failures is empty; expected >=1 entry")
	}
	if !strings.Contains(res.Failures[0], "header mismatch") {
		t.Errorf("res.Failures[0] = %q, want substring \"header mismatch\"", res.Failures[0])
	}
}

// TestVerifyIntegrity_TransientErrorBubbles covers behavior V5: a non-
// ErrBlockNotFound HEAD error bubbles up unwrapped so the caller can
// distinguish "data missing" from "operator-retryable transient".
func TestVerifyIntegrity_TransientErrorBubbles(t *testing.T) {
	f := newIntegrityFixture(t)
	addLegacyFile(t, f.loopFixture, "a.bin", "/a.bin", [][]byte{largeChunk('a')})
	runFullMigration(t, f)

	transient := errors.New("simulated network blip")
	f.stub.headFn = func(_ context.Context, key string) (remote.HeadResult, error) {
		return remote.HeadResult{}, transient
	}

	_, err := verifyIntegrity(t.Context(), f.rt, migrateOptions{share: f.share})
	if err == nil {
		t.Fatal("verifyIntegrity: expected error, got nil")
	}
	// MUST NOT wrap as ErrIntegrityCheckFailed — that sentinel is
	// reserved for "data missing or header mismatch" so the caller's
	// fail-loud path (D-A8) can distinguish.
	if errors.Is(err, ErrIntegrityCheckFailed) {
		t.Errorf("verifyIntegrity err is ErrIntegrityCheckFailed; transient errors must bubble unwrapped")
	}
	if !errors.Is(err, transient) {
		t.Errorf("verifyIntegrity err = %v, want wrapping %v", err, transient)
	}
}

// TestVerifyIntegrity_ConcurrencyHonorsParallel covers behavior V6: with
// --parallel=4, peak in-flight HEADs is at most 4 (the worker pool
// bounds concurrency).
func TestVerifyIntegrity_ConcurrencyHonorsParallel(t *testing.T) {
	f := newIntegrityFixture(t)
	// Many distinct files so we have plenty of distinct hashes; the
	// distinct-data-per-file pattern produces independent chunks
	// (deterministic large chunks with unique seeds).
	for i := 0; i < 16; i++ {
		buf := make([]byte, 4*1024*1024)
		for j := range buf {
			buf[j] = byte(i + 1)
		}
		addLegacyFile(t, f.loopFixture, byteSeqName(i), "/seq/"+byteSeqName(i), [][]byte{buf})
	}
	runFullMigration(t, f)

	// Inject a small delay on each HEAD so the goroutine pool has to
	// actually queue work — without the sleep all goroutines could
	// finish before the next one starts and maxInFlight would be 1.
	f.stub.headFn = func(_ context.Context, key string) (remote.HeadResult, error) {
		time.Sleep(20 * time.Millisecond)
		return f.stub.Store.HeadObject(t.Context(), key)
	}

	const wantParallel = 4
	res, err := verifyIntegrity(t.Context(), f.rt, migrateOptions{share: f.share, parallel: wantParallel})
	if err != nil {
		t.Fatalf("verifyIntegrity: %v", err)
	}
	if res.UniqueHashes < wantParallel {
		t.Skipf("not enough unique hashes (%d) to exercise parallelism", res.UniqueHashes)
	}
	got := f.stub.MaxInFlight()
	if got > wantParallel {
		t.Errorf("max in-flight HEADs = %d, want <= %d", got, wantParallel)
	}
	if got < 2 {
		t.Errorf("max in-flight HEADs = %d, want >= 2 (proves dispatch was concurrent)", got)
	}
}

// byteSeqName generates a unique file name from an int.
func byteSeqName(i int) string {
	return "f" + string(rune('0'+(i/10))) + string(rune('0'+(i%10))) + ".bin"
}

// Compile-time sanity: stubRemoteStore is wired in.
var _ atomic.Int64 // pin atomic import in case the stub stops using it later
