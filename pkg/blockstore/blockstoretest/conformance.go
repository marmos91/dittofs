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
	original := []byte("conformance: Put_Get_Roundtrip payload bytes")
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
