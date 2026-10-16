package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
)

// fakeCoordinator records every IncrementRefCount/DecrementRefCountAndReap
// call so engine tests can assert what the engine invoked (and with what
// arguments) without coupling to the real metadata-store wiring.
type fakeCoordinator struct {
	mu sync.Mutex

	incHashes []block.ContentHash
	reapIDs   []string

	// Optional: failOnNthIncrement returns an error on the Nth (1-based)
	// IncrementRefCount call. Zero disables.
	failOnNthIncrement int
	incCallCount       int

	// Optional: incErr overrides the error returned by failOnNthIncrement.
	// When nil the generic induced-failure error is used. Set to
	// block.ErrFileChunkNotFound to simulate a Pending (non-durable) donor
	// whose Remote-gated GetByHash resolves to no FileChunk row (#1245 Bug A).
	incErr error

	// Optional: incAllNotFound makes EVERY IncrementRefCount return
	// block.ErrFileChunkNotFound, modelling the real coordinator on a CAS
	// share where all source blocks are Pending and the Remote-gated
	// GetByHash never resolves them (#1384 CLONE / SMB server-side-copy).
	incAllNotFound bool

	// Optional: failOnNthDecrement returns an error on the Nth (1-based)
	// DecrementRefCount call. Zero disables.
	failOnNthDecrement int
	decCallCount       int
}

func (f *fakeCoordinator) IncrementRefCount(_ context.Context, hash block.ContentHash) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.incCallCount++
	if f.incAllNotFound {
		return block.ErrFileChunkNotFound
	}
	if f.failOnNthIncrement > 0 && f.incCallCount == f.failOnNthIncrement {
		if f.incErr != nil {
			return f.incErr
		}
		return errors.New("fakeCoordinator: induced IncrementRefCount failure")
	}
	f.incHashes = append(f.incHashes, hash)
	return nil
}

// errInducedDecrement is a sentinel returned by failOnNthDecrement so
// tests can assert errors.Is on the rollback path.
var errInducedDecrement = errors.New("fakeCoordinator: induced decrement failure")

// DecrementRefCountAndReap records reap-path invocations (engine Delete /
// Truncate reclaim) separately from the increment path (dedup / rollback
// bookkeeping) so tests can assert which path the engine took. The reap path is
// keyed by EXACT ID "{payloadID}/{offset}" (never by hash), so the recorded
// reapIDs reflect the unambiguous per-file row identity. Honours the same
// failOnNthDecrement injection so rollback-on-error tests still fire.
func (f *fakeCoordinator) DecrementRefCountAndReap(_ context.Context, payloadID string, offset uint64) (uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decCallCount++
	if f.failOnNthDecrement > 0 && f.decCallCount == f.failOnNthDecrement {
		return 0, errInducedDecrement
	}
	f.reapIDs = append(f.reapIDs, fmt.Sprintf("%s/%d", payloadID, offset))
	return 0, nil
}

// ReprojectBlocks is a no-op: this fake does not model the Blocks
// projection.
func (f *fakeCoordinator) ReprojectBlocks(_ context.Context, _ string) error { return nil }

// Compile-time assertion: fakeCoordinator satisfies MetadataCoordinator.
var _ MetadataCoordinator = (*fakeCoordinator)(nil)

// TestMetadataCoordinator_NilTolerated asserts that a *Store can be
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

// TestMetadataCoordinator_AcceptsFakeCoordinator asserts that a *Store
// can be constructed with a non-nil coordinator and that the field is
// stored verbatim. Full integration assertions (the engine actually
// invoking IncrementRefCount/DecrementRefCountAndReap during
// CopyPayload/Delete/Truncate) live in Task 2 tests.
func TestMetadataCoordinator_AcceptsFakeCoordinator(t *testing.T) {
	fc := &fakeCoordinator{}
	bs := newTestEngineWithCoordinator(t, fc)

	if bs.coordinator == nil {
		t.Fatal("Store.coordinator is nil after construction with non-nil fakeCoordinator")
	}
	if bs.coordinator != MetadataCoordinator(fc) {
		t.Fatal("Store.coordinator does not equal injected fakeCoordinator")
	}
}

// TestMetadataCoordinator_FakeImpl_RecordsCalls is a smoke test for the
// fake itself; full integration tests come in Task 2.
func TestMetadataCoordinator_FakeImpl_RecordsCalls(t *testing.T) {
	fc := &fakeCoordinator{}
	ctx := context.Background()

	h1 := block.ContentHash{0xAA}

	if err := fc.IncrementRefCount(ctx, h1); err != nil {
		t.Fatalf("Increment: %v", err)
	}
	if _, err := fc.DecrementRefCountAndReap(ctx, "pid", 0); err != nil {
		t.Fatalf("Decrement: %v", err)
	}

	if len(fc.incHashes) != 1 || fc.incHashes[0] != h1 {
		t.Errorf("incHashes=%v, want [h1]", fc.incHashes)
	}
	if len(fc.reapIDs) != 1 || fc.reapIDs[0] != "pid/0" {
		t.Errorf("reapIDs=%v, want [pid/0]", fc.reapIDs)
	}
}

// TestMetadataCoordinator_FakeImpl_FailureModes asserts the failOnNth* gates
// surface induced errors at the expected position so Task 2 mid-loop
// rollback tests can rely on the contract.
func TestMetadataCoordinator_FakeImpl_FailureModes(t *testing.T) {
	fc := &fakeCoordinator{failOnNthIncrement: 2}
	ctx := context.Background()

	if err := fc.IncrementRefCount(ctx, block.ContentHash{0x01}); err != nil {
		t.Fatalf("Increment 1: %v", err)
	}
	if err := fc.IncrementRefCount(ctx, block.ContentHash{0x02}); err == nil {
		t.Fatal("Increment 2: expected induced failure, got nil")
	}
	if err := fc.IncrementRefCount(ctx, block.ContentHash{0x03}); err != nil {
		t.Fatalf("Increment 3 (after failure): %v", err)
	}
	if len(fc.incHashes) != 2 {
		t.Errorf("incHashes recorded=%d (want 2 — failure on call 2 should not record)", len(fc.incHashes))
	}
}

func (f *fakeCoordinator) DecrementRefCountAndReapMany(ctx context.Context, payloadID string, offsets []uint64) error {
	return reapEach(ctx, payloadID, offsets, f.DecrementRefCountAndReap)
}

// reapEach implements the batched reap as a loop over the single-offset form,
// so a test double keeps whatever bookkeeping and error injection that form
// already carries.
func reapEach(
	ctx context.Context,
	payloadID string,
	offsets []uint64,
	reap func(context.Context, string, uint64) (uint32, error),
) error {
	for _, offset := range offsets {
		if _, err := reap(ctx, payloadID, offset); err != nil {
			return err
		}
	}
	return nil
}
