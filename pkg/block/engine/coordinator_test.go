package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
)

// fakeCoordinator records every IncrementRefCount/DecrementRefCount/
// PersistFileBlocks/FindByObjectID call so engine tests can assert what
// the engine invoked (and with what arguments) without coupling to the
// real metadata-store wiring.
type fakeCoordinator struct {
	mu sync.Mutex

	incHashes    []block.ContentHash
	decHashes    []block.ContentHash
	reapIDs      []string
	persistCalls []persistRecord

	// Optional: failOnNthIncrement returns an error on the Nth (1-based)
	// IncrementRefCount call. Zero disables.
	failOnNthIncrement int
	incCallCount       int

	// Optional: failOnNthDecrement returns an error on the Nth (1-based)
	// DecrementRefCount call. Zero disables.
	failOnNthDecrement int
	decCallCount       int

	// short-circuit support (RED tests).
	//
	// findCalls records every ObjectID looked up via FindByObjectID so
	// tests can assert call sequence + count.
	findCalls []block.ObjectID
	// objectIDPersisted records the ObjectID arg passed on every
	// PersistFileBlocks call (parallel index to persistCalls). Tests
	// assert the short-circuit threads the provisional ObjectID through
	// the same metadata txn that writes Blocks.
	objectIDPersisted []block.ObjectID
	// objectIDHits is the seedable hit-table: when FindByObjectID is
	// invoked with an ObjectID present here, returns the canned BlockRef
	// list (deep-copied on the way out to keep slice-aliasing discipline
	// per). Empty / unset key means miss → (nil, nil).
	objectIDHits map[block.ObjectID][]block.BlockRef
	// persistErr is a single-shot injection: the next PersistFileBlocks
	// call returns this error and clears the field. Used by the
	// concurrent-race RED test to simulate the loser detecting a
	// unique-violation on the partial UNIQUE index.
	persistErr error
	// fileObjectIDs is the seedable per-payload current-ObjectID lookup
	// for GetFileObjectID.: Syncer.Flush
	// reads this to evaluate the trigger condition before the
	// per-block upload pump. Unset / zero-valued entries mean "never
	// quiesced" (zero ObjectID), which lets the trigger condition fire
	// when speculativeBlocks are present and all-Pending.
	fileObjectIDs map[string]block.ObjectID

	// getFileObjectIDCalls counts GetFileObjectID invocations.
	// (Opt 4): the eager small-file dedup fast-path runs BEFORE
	// the speculative-dedup hook in engine.Flush and does NOT consult
	// GetFileObjectID; the speculative hook DOES. A zero count after a
	// Flush therefore proves the eager path short-circuited (the
	// speculative branch was skipped); a count of 1 proves the eager
	// path missed/bypassed and the speculative hook ran.
	getFileObjectIDCalls int
}

// newFakeCoordinator returns a *fakeCoordinator with all maps initialized
// so tests can seed objectIDHits without a separate make() call. Tests
// that don't need short-circuit support may continue to use
// `&fakeCoordinator{}` directly — the FindByObjectID code paths handle a
// nil map.
func newFakeCoordinator() *fakeCoordinator {
	return &fakeCoordinator{
		objectIDHits:  make(map[block.ObjectID][]block.BlockRef),
		fileObjectIDs: make(map[string]block.ObjectID),
	}
}

type persistRecord struct {
	payloadID string
	blocks    []block.BlockRef
	objectID  block.ObjectID
}

func (f *fakeCoordinator) IncrementRefCount(_ context.Context, hash block.ContentHash) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.incCallCount++
	if f.failOnNthIncrement > 0 && f.incCallCount == f.failOnNthIncrement {
		return errors.New("fakeCoordinator: induced IncrementRefCount failure")
	}
	f.incHashes = append(f.incHashes, hash)
	return nil
}

// errInducedDecrement is a sentinel returned by failOnNthDecrement so
// tests can assert errors.Is on the rollback path.
var errInducedDecrement = errors.New("fakeCoordinator: induced DecrementRefCount failure")

func (f *fakeCoordinator) DecrementRefCount(_ context.Context, hash block.ContentHash) (uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decCallCount++
	if f.failOnNthDecrement > 0 && f.decCallCount == f.failOnNthDecrement {
		return 0, errInducedDecrement
	}
	f.decHashes = append(f.decHashes, hash)
	return 0, nil
}

// DecrementRefCountAndReap records reap-path invocations (engine Delete /
// Truncate reclaim) separately from plain DecrementRefCount (dedup / rollback
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

func (f *fakeCoordinator) GetPersistedBlocks(_ context.Context, _ string) ([]block.BlockRef, error) {
	return nil, nil
}

func (f *fakeCoordinator) PersistFileBlocks(_ context.Context, payloadID string, blocks []block.BlockRef, objectID block.ObjectID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := append([]block.BlockRef(nil), blocks...)
	f.persistCalls = append(f.persistCalls, persistRecord{payloadID: payloadID, blocks: cp, objectID: objectID})
	f.objectIDPersisted = append(f.objectIDPersisted, objectID)
	if f.persistErr != nil {
		err := f.persistErr
		f.persistErr = nil // single-shot
		return err
	}
	return nil
}

// FindByObjectID —. Records every lookup
// in findCalls and returns a deep-copied BlockRef slice when the
// ObjectID is present in the seeded objectIDHits map. Miss returns
// (nil, nil). The deep copy preserves slice-aliasing
// discipline so tests cannot accidentally mutate seeded state.
func (f *fakeCoordinator) FindByObjectID(_ context.Context, oid block.ObjectID) ([]block.BlockRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.findCalls = append(f.findCalls, oid)
	if hit, ok := f.objectIDHits[oid]; ok {
		return append([]block.BlockRef(nil), hit...), nil
	}
	return nil, nil
}

// GetFileObjectID —. Returns the seeded
// ObjectID for payloadID from the fileObjectIDs map, or the all-zero
// sentinel + nil when the map is unset / payload absent. Mirrors the
// runtime coordinator's "no row → zero ObjectID" disposition so unit
// tests exercise the same trigger-condition code path as production.
func (f *fakeCoordinator) GetFileObjectID(_ context.Context, payloadID string) (block.ObjectID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getFileObjectIDCalls++
	if oid, ok := f.fileObjectIDs[payloadID]; ok {
		return oid, nil
	}
	return block.ObjectID{}, nil
}

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
// invoking IncrementRefCount/DecrementRefCount/PersistFileBlocks during
// CopyPayload/Delete/Truncate/syncer-post-Flush) live in Task 2 tests.
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
	h2 := block.ContentHash{0xBB}

	if err := fc.IncrementRefCount(ctx, h1); err != nil {
		t.Fatalf("Increment: %v", err)
	}
	if _, err := fc.DecrementRefCount(ctx, h2); err != nil {
		t.Fatalf("Decrement: %v", err)
	}
	if err := fc.PersistFileBlocks(ctx, "pid", []block.BlockRef{{Hash: h1, Offset: 0, Size: 1}}, block.ObjectID{}); err != nil {
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
