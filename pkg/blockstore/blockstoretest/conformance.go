package blockstoretest

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// Factory creates a fresh BlockStore for a single conformance subtest
// along with a cleanup closure. Each subtest invokes the factory and
// registers cleanup via t.Cleanup so subtests do not share state.
//
// The cleanup closure is responsible for closing the store and removing
// any backing storage (e.g. tempdir teardown for the fs backend). It
// MUST be safe to invoke when the store has not yet performed any I/O.
//
// Factory is intentionally a defined type (not a type alias) so the
// conformance suite can refer to it by name in godoc and so backends
// document the contract explicitly when constructing the factory.
type Factory func(t *testing.T) (blockstore.BlockStore, func())

// BlockStoreConformance runs the unified contract suite
// against any BlockStore implementation. Backends call this from their
// own _test.go files via a backend-specific factory.
//
// The scenarios pin the contract documented on the BlockStore interface
// in pkg/blockstore/blockstore.go
//
//   - Put + Get round-trip with no-aliasing of internal storage.
//   - Get on an unstored hash returns blockstore.ErrChunkNotFound.
//   - GetRange returns the requested byte sub-range.
//   - Delete is durable and is observable via a subsequent Get miss.
//   - Walk enumerates every object with non-zero LastModified.
//   - Walk callback may return blockstore.ErrStopWalk to exit cleanly
//     (Walk returns nil); any other non-nil error halts and is wrapped
//
// "walk halted at %s: %w".
//   - Head returns Meta whose Size matches Get's body length and whose
//
// LastModified is non-zero (carry-forward).
//   - Put is idempotent under same-hash same-bytes.
//   - Put is safe under concurrent same-hash same-bytes writers.
//   - Put on a zero-byte payload is accepted; Has/Get/Head report it.
//   - GetRange past EOF returns either an error (offset >= EOF) or the
//     available tail (offset < EOF, offset+length > EOF). Backends MAY
//     diverge on which error sentinel they wrap, so the suite asserts
//     "any non-nil error" for the offset >= EOF case rather than pinning
//     a specific sentinel — see the GetRange godoc on BlockStore which
//     explicitly permits both clamp and explicit-error behaviors.
//   - Concurrent Put + Walk never surfaces a duplicate hash from Walk;
//     in-flight Puts MAY be invisible to Walk but any hash Walk does
//     observe is observed at most once.
//   - Put accepts a payload whose bytes do not match the supplied hash:
//     the no-verify-on-Put contract is the caller's responsibility, and
//     Get returns whatever bytes were stored under that key.
//
// Backends that also implement BlockStoreAppend additionally call
// BlockStoreAppendConformance.
func BlockStoreConformance(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("Put_Get_Roundtrip", func(t *testing.T) { testPutGetRoundtrip(t, factory) })
	t.Run("Get_NotFound", func(t *testing.T) { testGetNotFound(t, factory) })
	t.Run("GetRange", func(t *testing.T) { testGetRange(t, factory) })
	t.Run("Delete", func(t *testing.T) { testDelete(t, factory) })
	t.Run("Walk", func(t *testing.T) { testWalk(t, factory) })
	t.Run("Walk_ErrStopWalk", func(t *testing.T) { testWalkStopSentinel(t, factory) })
	t.Run("Walk_ErrorWrap", func(t *testing.T) { testWalkErrorWrap(t, factory) })
	t.Run("Head", func(t *testing.T) { testHead(t, factory) })
	t.Run("Put_Idempotent_SameHash", func(t *testing.T) { testPutIdempotent(t, factory) })
	t.Run("Put_Concurrent_SameHash", func(t *testing.T) { testPutConcurrent(t, factory) })
	t.Run("Put_ZeroByte", func(t *testing.T) { testPutZeroByte(t, factory) })
	t.Run("GetRange_PastEOF", func(t *testing.T) { testGetRangePastEOF(t, factory) })
	t.Run("GetRange_InvalidBounds", func(t *testing.T) { testGetRangeInvalidBounds(t, factory) })
	t.Run("Concurrent_Put_Walk_NoDuplicates", func(t *testing.T) { testConcurrentPutWalkNoDuplicates(t, factory) })
	t.Run("Put_WrongHash_NoVerify", func(t *testing.T) { testPutWrongHashNoVerify(t, factory) })
}

// blake3Sum is the conformance suite's shared hashing helper. It mirrors
// blake3ContentHash at pkg/blockstore/local/fs/rollup.go:449 — the
// rollup loop hashes chunk bytes the same way before storing them, so
// the suite's fixtures share the rollup's content-address contract.
// Shared with appendlog.go (same package, no import needed).
func blake3Sum(b []byte) blockstore.ContentHash {
	var h blockstore.ContentHash
	sum := blake3.Sum256(b)
	copy(h[:], sum[:])
	return h
}

func testPutGetRoundtrip(t *testing.T, factory Factory) {
	bs, cleanup := factory(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	// Keep a private copy of the original bytes so the aliasing check
	// below is meaningful — never trust the slice that was handed to
	// Put after the Put returns; backends may reference it during
	// asynchronous flushes.
	original := []byte("conformance: hello blockstore round-trip payload")
	stored := make([]byte, len(original))
	copy(stored, original)

	h := blake3Sum(stored)
	if err := bs.Put(ctx, h, stored); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got1, err := bs.Get(ctx, h)
	if err != nil {
		t.Fatalf("Get #1: %v", err)
	}
	if !bytes.Equal(got1, original) {
		t.Fatalf("Get #1 returned bytes that differ from stored payload")
	}

	// Aliasing defense (carry-forward): mutate the slice
	// returned by the first Get and confirm a fresh Get returns the
	// unchanged bytes. Backends MUST NOT alias internal storage on
	// reads — the Cache copy-out invariant depends on it.
	got1[0] ^= 0xFF
	got2, err := bs.Get(ctx, h)
	if err != nil {
		t.Fatalf("Get #2: %v", err)
	}
	if !bytes.Equal(got2, original) {
		t.Fatalf("Get is aliasing: mutating the first Get slice changed the second Get slice")
	}
}

func testGetNotFound(t *testing.T, factory Factory) {
	bs, cleanup := factory(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	// Non-zero arbitrary hash so the assertion does not collide with any
	// hypothetical "all-zero hash" sentinel a backend might special-case.
	var missing blockstore.ContentHash
	missing[0] = 0xDE
	missing[31] = 0xAD

	data, err := bs.Get(ctx, missing)
	if err == nil {
		t.Fatalf("Get on missing hash: expected error, got nil (data len=%d)", len(data))
	}
	// BlockStore contract: missing-key reads return ErrChunkNotFound —
	// see blockstore.go Get godoc.
	if !errors.Is(err, blockstore.ErrChunkNotFound) {
		t.Fatalf("Get on missing hash: want ErrChunkNotFound, got %v", err)
	}
	if data != nil {
		t.Fatalf("Get on missing hash: want nil data, got %d bytes", len(data))
	}
}

func testGetRange(t *testing.T, factory Factory) {
	bs, cleanup := factory(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	stored := []byte("0123456789abcdef") // exactly 16 bytes
	h := blake3Sum(stored)
	if err := bs.Put(ctx, h, stored); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := bs.GetRange(ctx, h, 4, 8)
	if err != nil {
		t.Fatalf("GetRange: %v", err)
	}
	want := stored[4:12]
	if !bytes.Equal(got, want) {
		t.Fatalf("GetRange returned %q, want %q", got, want)
	}
}

func testDelete(t *testing.T, factory Factory) {
	bs, cleanup := factory(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	stored := []byte("delete-then-get fixture")
	h := blake3Sum(stored)
	if err := bs.Put(ctx, h, stored); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := bs.Delete(ctx, h); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, err := bs.Get(ctx, h); !errors.Is(err, blockstore.ErrChunkNotFound) {
		t.Fatalf("Get after Delete: want ErrChunkNotFound, got %v", err)
	}
}

func testWalk(t *testing.T, factory Factory) {
	bs, cleanup := factory(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	payloads := [][]byte{
		[]byte("walk-payload-1: " + string(bytes.Repeat([]byte{0xAA}, 64))),
		[]byte("walk-payload-2: " + string(bytes.Repeat([]byte{0xBB}, 128))),
		[]byte("walk-payload-3: " + string(bytes.Repeat([]byte{0xCC}, 256))),
	}
	want := make(map[blockstore.ContentHash]int64, len(payloads))
	for _, p := range payloads {
		h := blake3Sum(p)
		want[h] = int64(len(p))
		if err := bs.Put(ctx, h, p); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	seen := make(map[blockstore.ContentHash]blockstore.Meta, len(payloads))
	err := bs.Walk(ctx, func(h blockstore.ContentHash, m blockstore.Meta) error {
		seen[h] = m
		return nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(seen) != len(want) {
		t.Fatalf("Walk visited %d objects, want %d", len(seen), len(want))
	}
	for h, wantSize := range want {
		m, ok := seen[h]
		if !ok {
			t.Errorf("Walk did not visit hash %s", h)
			continue
		}
		if m.Size != wantSize {
			t.Errorf("Walk Meta.Size for %s = %d, want %d", h, m.Size, wantSize)
		}
		if m.LastModified.IsZero() {
			t.Errorf("Walk Meta.LastModified for %s is zero (contract violation)", h)
		}
	}
}

func testWalkStopSentinel(t *testing.T, factory Factory) {
	bs, cleanup := factory(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	// Seed three objects. The callback returns ErrStopWalk after the
	// first invocation and the contract requires Walk to return
	// nil to the outer caller AND to not invoke the callback again.
	for _, p := range [][]byte{[]byte("a"), []byte("b"), []byte("c")} {
		h := blake3Sum(p)
		if err := bs.Put(ctx, h, p); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	seen := 0
	err := bs.Walk(ctx, func(h blockstore.ContentHash, m blockstore.Meta) error {
		seen++
		return blockstore.ErrStopWalk
	})
	if err != nil {
		t.Fatalf("Walk on ErrStopWalk: want nil, got %v", err)
	}
	if seen != 1 {
		t.Fatalf("Walk should have stopped after the first ErrStopWalk; callback invoked %d times", seen)
	}
}

// testWalkErrorWrap pins the "non-ErrStopWalk error halts and is
// wrapped 'walk halted at %s: %w'" contract: the suite returns a custom
// sentinel from the callback and asserts the Walk's return error
// matches via errors.Is.
func testWalkErrorWrap(t *testing.T, factory Factory) {
	bs, cleanup := factory(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	for _, p := range [][]byte{[]byte("x"), []byte("y"), []byte("z")} {
		h := blake3Sum(p)
		if err := bs.Put(ctx, h, p); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	customErr := errors.New("conformance: walk-callback custom error")
	seen := 0
	err := bs.Walk(ctx, func(h blockstore.ContentHash, m blockstore.Meta) error {
		seen++
		return customErr
	})
	if err == nil {
		t.Fatalf("Walk on custom callback error: want non-nil, got nil")
	}
	if !errors.Is(err, customErr) {
		t.Fatalf("Walk error does not wrap custom callback error: got %v (want errors.Is == customErr)", err)
	}
	if seen != 1 {
		t.Fatalf("Walk should have halted after the first error; callback invoked %d times", seen)
	}
}

func testHead(t *testing.T, factory Factory) {
	bs, cleanup := factory(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	stored := bytes.Repeat([]byte{0x42}, 100)
	h := blake3Sum(stored)
	if err := bs.Put(ctx, h, stored); err != nil {
		t.Fatalf("Put: %v", err)
	}

	m, err := bs.Head(ctx, h)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if m.Size != int64(len(stored)) {
		t.Errorf("Head Meta.Size = %d, want %d", m.Size, len(stored))
	}
	if m.LastModified.IsZero() {
		t.Error("Head Meta.LastModified is zero (contract violation)")
	}
}

func testPutIdempotent(t *testing.T, factory Factory) {
	bs, cleanup := factory(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	stored := []byte("idempotent same-hash same-bytes")
	h := blake3Sum(stored)

	if err := bs.Put(ctx, h, stored); err != nil {
		t.Fatalf("Put #1: %v", err)
	}
	if err := bs.Put(ctx, h, stored); err != nil {
		t.Fatalf("Put #2 (idempotent): %v", err)
	}

	count := 0
	err := bs.Walk(ctx, func(_ blockstore.ContentHash, _ blockstore.Meta) error {
		count++
		return nil
	})
	if err != nil {
		t.Fatalf("Walk after idempotent Put: %v", err)
	}
	if count != 1 {
		t.Fatalf("idempotent Put created %d objects, want 1", count)
	}
}

func testPutConcurrent(t *testing.T, factory Factory) {
	bs, cleanup := factory(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	stored := []byte("concurrent same-hash same-bytes payload")
	h := blake3Sum(stored)

	const writers = 8
	var wg sync.WaitGroup
	wg.Add(writers)
	errCh := make(chan error, writers)
	for i := 0; i < writers; i++ {
		go func() {
			defer wg.Done()
			// Each goroutine writes its own copy so backends that
			// reference the slice across the Put boundary cannot
			// observe a cross-goroutine mutation.
			buf := make([]byte, len(stored))
			copy(buf, stored)
			if err := bs.Put(ctx, h, buf); err != nil {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent Put returned error: %v", err)
	}

	count := 0
	err := bs.Walk(ctx, func(_ blockstore.ContentHash, _ blockstore.Meta) error {
		count++
		return nil
	})
	if err != nil {
		t.Fatalf("Walk after concurrent Put: %v", err)
	}
	if count != 1 {
		t.Fatalf("concurrent same-hash Put created %d objects, want 1", count)
	}
}

// testPutZeroByte pins the contract that backends MUST accept a
// zero-byte payload. The empty BLAKE3 digest is a well-known fixture
// (af1349b9...3262) — backends that reject empty data would break
// callers that legitimately address an empty chunk (e.g., the tail of a
// file whose final boundary lands at offset 0). Has/Get/Head all MUST
// reflect the stored object.
func testPutZeroByte(t *testing.T, factory Factory) {
	bs, cleanup := factory(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	empty := []byte{}
	h := blake3Sum(empty)
	if err := bs.Put(ctx, h, empty); err != nil {
		t.Fatalf("Put zero-byte: %v", err)
	}

	has, err := bs.Has(ctx, h)
	if err != nil {
		t.Fatalf("Has after zero-byte Put: %v", err)
	}
	if !has {
		t.Fatalf("Has after zero-byte Put: want true, got false")
	}

	got, err := bs.Get(ctx, h)
	if err != nil {
		t.Fatalf("Get after zero-byte Put: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Get after zero-byte Put: want zero-length slice, got %d bytes", len(got))
	}

	m, err := bs.Head(ctx, h)
	if err != nil {
		t.Fatalf("Head after zero-byte Put: %v", err)
	}
	if m.Size != 0 {
		t.Errorf("Head Meta.Size = %d, want 0", m.Size)
	}
	if m.LastModified.IsZero() {
		t.Error("Head Meta.LastModified is zero (contract violation)")
	}
}

// testGetRangePastEOF pins the cross-backend GetRange-past-EOF contract.
// The BlockStore.GetRange godoc explicitly permits backends to either
// return a clamped tail OR return an explicit error, so the conformance
// suite asserts the union of valid behaviors:
//
//   - offset >= EOF MUST surface a non-nil error. The specific sentinel
//     varies by backend (FSStore wraps a plain fmt.Errorf, remote/memory
//     historically returned ErrChunkNotFound, the decorators return
//     ErrInvalidOffset), so the suite checks only "any error" rather
//     than pinning a sentinel. CS-2 in the v1.0 audit (REVIEW.md §4)
//     flagged this divergence — aligning the sentinel across backends
//     is tracked separately and out of scope for this conformance PR.
//
//   - offset < EOF but offset+length > EOF MUST return the available
//     bytes (offset..EOF) without error. Every existing backend already
//     clamps, so this is the safe contract to pin.
func testGetRangePastEOF(t *testing.T, factory Factory) {
	bs, cleanup := factory(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	stored := []byte("0123456789abcdef") // exactly 16 bytes
	h := blake3Sum(stored)
	if err := bs.Put(ctx, h, stored); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// (a) offset strictly past EOF: contract requires a non-nil error.
	got, err := bs.GetRange(ctx, h, 20, 8)
	if err == nil {
		t.Fatalf("GetRange offset=20 length=8 (past EOF): want non-nil error, got nil (data len=%d)", len(got))
	}

	// (b) partial past EOF: contract requires the available tail. The
	// returned slice is the byte range [offset, EOF) — callers detect
	// the short read by comparing returned len against requested length.
	got, err = bs.GetRange(ctx, h, 8, 20)
	if err != nil {
		t.Fatalf("GetRange offset=8 length=20 (partial past EOF): want nil error, got %v", err)
	}
	want := stored[8:]
	if !bytes.Equal(got, want) {
		t.Fatalf("GetRange partial-past-EOF returned %q, want %q", got, want)
	}
}

// testGetRangeInvalidBounds pins the malformed-argument sentinels on the
// BlockStore.GetRange contract: a negative offset MUST surface
// blockstore.ErrInvalidOffset and a non-positive length MUST surface
// blockstore.ErrInvalidSize. Unlike the offset-past-EOF case (which the
// godoc permits backends to report with any error), these two arguments
// are unconditionally invalid regardless of object size, so every
// backend can detect them before touching storage and report the exact
// sentinel via errors.Is.
func testGetRangeInvalidBounds(t *testing.T, factory Factory) {
	bs, cleanup := factory(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	stored := []byte("0123456789abcdef") // exactly 16 bytes
	h := blake3Sum(stored)
	if err := bs.Put(ctx, h, stored); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Negative offset is ErrInvalidOffset.
	if _, err := bs.GetRange(ctx, h, -1, 4); !errors.Is(err, blockstore.ErrInvalidOffset) {
		t.Fatalf("GetRange offset=-1: want ErrInvalidOffset, got %v", err)
	}

	// Zero and negative length are ErrInvalidSize.
	if _, err := bs.GetRange(ctx, h, 0, 0); !errors.Is(err, blockstore.ErrInvalidSize) {
		t.Fatalf("GetRange length=0: want ErrInvalidSize, got %v", err)
	}
	if _, err := bs.GetRange(ctx, h, 0, -4); !errors.Is(err, blockstore.ErrInvalidSize) {
		t.Fatalf("GetRange length=-4: want ErrInvalidSize, got %v", err)
	}
}

// testConcurrentPutWalkNoDuplicates pins the "Walk surfaces every hash
// at most once" invariant. The S3 paginator can in principle surface a
// duplicate hash if a key crosses a pagination boundary mid-list and
// the next page's continuation token is mishandled; the FSStore and
// memory backends are immune by construction. In-flight Puts are NOT
// required to appear in Walk's snapshot — partial visibility is fine —
// but no hash may appear twice.
func testConcurrentPutWalkNoDuplicates(t *testing.T, factory Factory) {
	bs, cleanup := factory(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	const writers = 8

	// Seed distinct payloads up front so each writer has its own hash.
	payloads := make([][]byte, writers)
	hashes := make([]blockstore.ContentHash, writers)
	for i := 0; i < writers; i++ {
		p := []byte("concurrent-walk-payload-")
		p = append(p, byte('A'+i))
		p = append(p, bytes.Repeat([]byte{byte(i)}, 64)...)
		payloads[i] = p
		hashes[i] = blake3Sum(p)
	}

	var wg sync.WaitGroup
	wg.Add(writers + 1)
	errCh := make(chan error, writers+1)

	// Writer goroutines.
	for i := 0; i < writers; i++ {
		i := i
		go func() {
			defer wg.Done()
			if err := bs.Put(ctx, hashes[i], payloads[i]); err != nil {
				errCh <- err
			}
		}()
	}

	// Walker goroutine — runs concurrently with the writers.
	var walkMu sync.Mutex
	seen := make(map[blockstore.ContentHash]int)
	go func() {
		defer wg.Done()
		err := bs.Walk(ctx, func(h blockstore.ContentHash, _ blockstore.Meta) error {
			walkMu.Lock()
			seen[h]++
			walkMu.Unlock()
			return nil
		})
		if err != nil {
			errCh <- err
		}
	}()

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent Put + Walk returned error: %v", err)
	}

	walkMu.Lock()
	defer walkMu.Unlock()
	for h, n := range seen {
		if n > 1 {
			t.Errorf("Walk observed hash %s %d times during concurrent Put (want at most once)", h, n)
		}
	}
}

// testPutWrongHashNoVerify pins the no-verify-on-Put contract: backends
// MUST trust the caller-supplied hash and MUST NOT recompute or
// validate the digest at Put time. Verification is a read-side
// responsibility (e.g., the S3 read path's streaming BLAKE3 verifier in
// ReadBlockVerified). A Put with a payload whose bytes do not hash to
// the supplied key succeeds; a subsequent Get under that key returns
// the stored bytes verbatim. No ErrCASContentMismatch surfaces from
// Put — that sentinel is read-side only.
func testPutWrongHashNoVerify(t *testing.T, factory Factory) {
	bs, cleanup := factory(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	wrongHash := blake3Sum([]byte("foo"))
	payload := []byte("bar")
	if err := bs.Put(ctx, wrongHash, payload); err != nil {
		if errors.Is(err, blockstore.ErrCASContentMismatch) {
			t.Fatalf("Put with mismatched hash returned ErrCASContentMismatch — backends must not verify on Put: %v", err)
		}
		t.Fatalf("Put with mismatched hash: want success, got %v", err)
	}

	got, err := bs.Get(ctx, wrongHash)
	if err != nil {
		t.Fatalf("Get after mismatched-hash Put: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("Get after mismatched-hash Put: returned %q, want %q", got, payload)
	}
}
