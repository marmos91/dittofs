package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// Phase 12 D-37 (WR-4-01 follow-up) tests for the dedup short-circuit fix
// in engine.uploadOne. The donor row is canonical post-fix; the duplicate
// fb row that triggered uploadOne is deleted instead of being persisted as
// a second hash-collision row. INV-02 (∑FileBlock.RefCount ==
// ∑len(FileAttr.Blocks)) holds unconditionally for blocks written through
// this path.

// TestUploadOne_Dedup_SingleRowAfterShortCircuit asserts D-37: when
// uploadOne hits the dedup short-circuit, exactly one FileBlock row
// remains for the shared hash (the donor). The duplicate fb row that
// uploadOne was invoked on is deleted; the donor's RefCount is 2.
func TestUploadOne_Dedup_SingleRowAfterShortCircuit(t *testing.T) {
	m, _, ms, tmp := newUnitSyncer(t)
	ctx := context.Background()

	// Build a payload, compute its hash, and seed a donor that already
	// holds that hash in BlockStateRemote.
	payload := []byte("dedup-payload-bytes")
	h := blake3.Sum256(payload)
	var hash blockstore.ContentHash
	copy(hash[:], h[:])

	donor := &blockstore.FileBlock{
		ID:            "share/donor",
		Hash:          hash,
		BlockStoreKey: blockstore.FormatCASKey(hash),
		DataSize:      uint32(len(payload)),
		State:         blockstore.BlockStateRemote,
		RefCount:      1,
		LastAccess:    time.Now(),
		CreatedAt:     time.Now(),
	}
	if err := ms.Put(ctx, donor); err != nil {
		t.Fatalf("Put donor: %v", err)
	}

	// Seed a duplicate row (the syncer-claim path's provisional row). It
	// is in BlockStatePending → flipped to Syncing for uploadOne.
	dup := seedPendingBlock(t, ms, tmp, "dup", payload)
	dup.State = blockstore.BlockStateSyncing
	dup.LastSyncAttemptAt = time.Now()
	if err := ms.Put(ctx, dup); err != nil {
		t.Fatalf("Put dup(Syncing): %v", err)
	}

	if err := m.uploadOne(ctx, dup); err != nil {
		t.Fatalf("uploadOne: %v", err)
	}

	// Donor's RefCount must be 2.
	gotDonor, err := ms.GetFileBlock(ctx, donor.ID)
	if err != nil {
		t.Fatalf("GetFileBlock(donor): %v", err)
	}
	if gotDonor.RefCount != 2 {
		t.Errorf("donor.RefCount = %d, want 2", gotDonor.RefCount)
	}
	if gotDonor.State != blockstore.BlockStateRemote {
		t.Errorf("donor.State = %v, want Remote", gotDonor.State)
	}

	// Duplicate row must be gone.
	if _, err := ms.GetFileBlock(ctx, dup.ID); !errors.Is(err, metadata.ErrFileBlockNotFound) {
		t.Errorf("GetFileBlock(dup) err = %v, want ErrFileBlockNotFound", err)
	}

	// GetByHash must still resolve to a single row (the donor).
	got, err := ms.GetByHash(ctx, hash)
	if err != nil {
		t.Fatalf("GetByHash: %v", err)
	}
	if got == nil {
		t.Fatal("GetByHash returned nil for known-remote hash")
	}
	if got.ID != donor.ID {
		t.Errorf("GetByHash returned %s, want donor %s", got.ID, donor.ID)
	}
}

// TestUploadOne_Dedup_DonorRefCountIncrementedExactlyOnce asserts the
// RefCount accounting is correct: a single dedup short-circuit bumps the
// donor by exactly one, regardless of any subsequent code paths in
// uploadOne (in particular, no second bump from a follow-up Put).
func TestUploadOne_Dedup_DonorRefCountIncrementedExactlyOnce(t *testing.T) {
	m, _, ms, tmp := newUnitSyncer(t)
	ctx := context.Background()

	payload := []byte("dedup-once-payload")
	h := blake3.Sum256(payload)
	var hash blockstore.ContentHash
	copy(hash[:], h[:])

	donor := &blockstore.FileBlock{
		ID:            "share/donor-once",
		Hash:          hash,
		BlockStoreKey: blockstore.FormatCASKey(hash),
		DataSize:      uint32(len(payload)),
		State:         blockstore.BlockStateRemote,
		RefCount:      1,
		LastAccess:    time.Now(),
		CreatedAt:     time.Now(),
	}
	if err := ms.Put(ctx, donor); err != nil {
		t.Fatalf("Put donor: %v", err)
	}

	dup := seedPendingBlock(t, ms, tmp, "dup-once", payload)
	dup.State = blockstore.BlockStateSyncing
	dup.LastSyncAttemptAt = time.Now()
	if err := ms.Put(ctx, dup); err != nil {
		t.Fatalf("Put dup(Syncing): %v", err)
	}

	if err := m.uploadOne(ctx, dup); err != nil {
		t.Fatalf("uploadOne: %v", err)
	}

	gotDonor, err := ms.GetFileBlock(ctx, donor.ID)
	if err != nil {
		t.Fatalf("GetFileBlock(donor): %v", err)
	}
	// Exactly one bump (from 1 to 2).
	if gotDonor.RefCount != 2 {
		t.Fatalf("donor.RefCount = %d, want exactly 2", gotDonor.RefCount)
	}
}

// TestUploadOne_Dedup_DeleteIdempotent asserts the race-tolerant behavior
// of the post-fix dedup short-circuit: if a concurrent worker (or test
// injection) has already deleted the duplicate fb.ID before uploadOne's
// Delete call runs, ErrFileBlockNotFound from the underlying store is
// tolerated and uploadOne returns nil.
func TestUploadOne_Dedup_DeleteIdempotent(t *testing.T) {
	m, _, ms, tmp := newUnitSyncer(t)
	ctx := context.Background()

	payload := []byte("dedup-idempotent-payload")
	h := blake3.Sum256(payload)
	var hash blockstore.ContentHash
	copy(hash[:], h[:])

	donor := &blockstore.FileBlock{
		ID:            "share/donor-idem",
		Hash:          hash,
		BlockStoreKey: blockstore.FormatCASKey(hash),
		DataSize:      uint32(len(payload)),
		State:         blockstore.BlockStateRemote,
		RefCount:      1,
		LastAccess:    time.Now(),
		CreatedAt:     time.Now(),
	}
	if err := ms.Put(ctx, donor); err != nil {
		t.Fatalf("Put donor: %v", err)
	}

	dup := seedPendingBlock(t, ms, tmp, "dup-idem", payload)
	dup.State = blockstore.BlockStateSyncing
	dup.LastSyncAttemptAt = time.Now()
	if err := ms.Put(ctx, dup); err != nil {
		t.Fatalf("Put dup(Syncing): %v", err)
	}

	// Simulate a concurrent worker by deleting the row before uploadOne
	// runs. The dedup short-circuit's Delete will then see
	// ErrFileBlockNotFound, which the post-fix code tolerates.
	if err := ms.Delete(ctx, dup.ID); err != nil {
		t.Fatalf("simulate concurrent delete: %v", err)
	}

	if err := m.uploadOne(ctx, dup); err != nil {
		t.Fatalf("uploadOne with already-deleted dup: %v", err)
	}

	// Donor still bumped to 2.
	gotDonor, err := ms.GetFileBlock(ctx, donor.ID)
	if err != nil {
		t.Fatalf("GetFileBlock(donor): %v", err)
	}
	if gotDonor.RefCount != 2 {
		t.Errorf("donor.RefCount = %d, want 2", gotDonor.RefCount)
	}
}
