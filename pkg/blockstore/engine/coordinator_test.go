package engine

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// fakeCoordinator records every IncrementRefCount/DecrementRefCount/
// PersistFileBlocks call so engine tests can assert what the engine
// invoked (and with what arguments) without coupling to the real
// metadata-store wiring.
type fakeCoordinator struct {
	mu sync.Mutex

	incHashes    []blockstore.ContentHash
	decHashes    []blockstore.ContentHash
	persistCalls []persistRecord

	// Optional: failOnNthIncrement returns an error on the Nth (1-based)
	// IncrementRefCount call. Zero disables.
	failOnNthIncrement int
	incCallCount       int

	// Optional: failOnNthDecrement returns an error on the Nth (1-based)
	// DecrementRefCount call. Zero disables.
	failOnNthDecrement int
	decCallCount       int
}

type persistRecord struct {
	payloadID string
	blocks    []blockstore.BlockRef
}

func (f *fakeCoordinator) IncrementRefCount(_ context.Context, hash blockstore.ContentHash) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.incCallCount++
	if f.failOnNthIncrement > 0 && f.incCallCount == f.failOnNthIncrement {
		return errors.New("fakeCoordinator: induced IncrementRefCount failure")
	}
	f.incHashes = append(f.incHashes, hash)
	return nil
}

func (f *fakeCoordinator) DecrementRefCount(_ context.Context, hash blockstore.ContentHash) (uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decCallCount++
	if f.failOnNthDecrement > 0 && f.decCallCount == f.failOnNthDecrement {
		return 0, errors.New("fakeCoordinator: induced DecrementRefCount failure")
	}
	f.decHashes = append(f.decHashes, hash)
	return 0, nil
}

func (f *fakeCoordinator) PersistFileBlocks(_ context.Context, payloadID string, blocks []blockstore.BlockRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := append([]blockstore.BlockRef(nil), blocks...)
	f.persistCalls = append(f.persistCalls, persistRecord{payloadID: payloadID, blocks: cp})
	return nil
}

// Compile-time assertion: fakeCoordinator satisfies MetadataCoordinator.
var _ MetadataCoordinator = (*fakeCoordinator)(nil)

// TestMetadataCoordinator_NilTolerated asserts that a *BlockStore can be
// constructed with coordinator=nil (test ergonomics — production wiring
// always passes a real coordinator). The struct field exists and is
// nil; the ErrMetadataCoordinatorNotWired sentinel is non-nil and
// clearly named so callers can errors.Is() on it.
func TestMetadataCoordinator_NilTolerated(t *testing.T) {
	bs := newTestEngine(t, 0, 0)

	if bs == nil {
		t.Fatal("newTestEngine returned nil")
	}
	if bs.coordinator != nil {
		t.Fatalf("expected nil coordinator (test fixture), got %v", bs.coordinator)
	}
	if ErrMetadataCoordinatorNotWired == nil {
		t.Fatal("ErrMetadataCoordinatorNotWired is nil")
	}
	if ErrMetadataCoordinatorNotWired.Error() == "" {
		t.Fatal("ErrMetadataCoordinatorNotWired has empty message")
	}
}

// TestMetadataCoordinator_AcceptsFakeCoordinator asserts that a *BlockStore
// can be constructed with a non-nil coordinator and that the field is
// stored verbatim. Full integration assertions (the engine actually
// invoking IncrementRefCount/DecrementRefCount/PersistFileBlocks during
// CopyPayload/Delete/Truncate/syncer-post-Flush) live in Task 2 tests.
func TestMetadataCoordinator_AcceptsFakeCoordinator(t *testing.T) {
	fc := &fakeCoordinator{}
	bs := newTestEngineWithCoordinator(t, fc)

	if bs.coordinator == nil {
		t.Fatal("BlockStore.coordinator is nil after construction with non-nil fakeCoordinator")
	}
	if bs.coordinator != MetadataCoordinator(fc) {
		t.Fatal("BlockStore.coordinator does not equal injected fakeCoordinator")
	}
}

// TestMetadataCoordinator_FakeImpl_RecordsCalls is a smoke test for the
// fake itself; full integration tests come in Task 2.
func TestMetadataCoordinator_FakeImpl_RecordsCalls(t *testing.T) {
	fc := &fakeCoordinator{}
	ctx := context.Background()

	h1 := blockstore.ContentHash{0xAA}
	h2 := blockstore.ContentHash{0xBB}

	if err := fc.IncrementRefCount(ctx, h1); err != nil {
		t.Fatalf("Increment: %v", err)
	}
	if _, err := fc.DecrementRefCount(ctx, h2); err != nil {
		t.Fatalf("Decrement: %v", err)
	}
	if err := fc.PersistFileBlocks(ctx, "pid", []blockstore.BlockRef{{Hash: h1, Offset: 0, Size: 1}}); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	if len(fc.incHashes) != 1 || fc.incHashes[0] != h1 {
		t.Errorf("incHashes=%v, want [h1]", fc.incHashes)
	}
	if len(fc.decHashes) != 1 || fc.decHashes[0] != h2 {
		t.Errorf("decHashes=%v, want [h2]", fc.decHashes)
	}
	if len(fc.persistCalls) != 1 || fc.persistCalls[0].payloadID != "pid" {
		t.Errorf("persistCalls=%v, want one call with pid", fc.persistCalls)
	}
}

// TestMetadataCoordinator_FakeImpl_FailureModes asserts the failOnNth* gates
// surface induced errors at the expected position so Task 2 mid-loop
// rollback tests can rely on the contract.
func TestMetadataCoordinator_FakeImpl_FailureModes(t *testing.T) {
	fc := &fakeCoordinator{failOnNthIncrement: 2}
	ctx := context.Background()

	if err := fc.IncrementRefCount(ctx, blockstore.ContentHash{0x01}); err != nil {
		t.Fatalf("Increment 1: %v", err)
	}
	if err := fc.IncrementRefCount(ctx, blockstore.ContentHash{0x02}); err == nil {
		t.Fatal("Increment 2: expected induced failure, got nil")
	}
	if err := fc.IncrementRefCount(ctx, blockstore.ContentHash{0x03}); err != nil {
		t.Fatalf("Increment 3 (after failure): %v", err)
	}
	if len(fc.incHashes) != 2 {
		t.Errorf("incHashes recorded=%d (want 2 — failure on call 2 should not record)", len(fc.incHashes))
	}
}
