package storetest

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ===========================================================================
// Fault-injecting helpers for atomicity subtests
// ===========================================================================

// errSyncedLocatorsInjected is the sentinel returned by faultySyncedStore.
var errSyncedLocatorsInjected = errors.New("injected PutSyncedLocators failure")

// faultySyncedStore wraps a Store and makes the FIRST transactional
// PutSyncedLocators fail (DefaultCommitBlock records the chunks' synced markers
// inside the commit transaction), then delegates subsequent calls. CommitBlock
// delegates to metadata.DefaultCommitBlock with itself as the receiver so the
// injected transaction is actually exercised.
type faultySyncedStore struct {
	metadata.Store
	mu        sync.Mutex
	hasFailed bool
}

func (f *faultySyncedStore) WithTransaction(ctx context.Context, fn func(metadata.Transaction) error) error {
	return f.Store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return fn(&faultySyncedTx{Transaction: tx, parent: f})
	})
}

func (f *faultySyncedStore) CommitBlock(ctx context.Context, rec block.BlockRecord, chunks []block.BlockChunkCommit) error {
	return metadata.DefaultCommitBlock(ctx, f, rec, chunks, nil)
}

type faultySyncedTx struct {
	metadata.Transaction
	parent *faultySyncedStore
}

func (tx *faultySyncedTx) PutSyncedLocators(ctx context.Context, chunks []block.BlockChunkCommit) error {
	tx.parent.mu.Lock()
	first := !tx.parent.hasFailed
	tx.parent.hasFailed = true
	tx.parent.mu.Unlock()
	if first {
		return errSyncedLocatorsInjected
	}
	return tx.Transaction.PutSyncedLocators(ctx, chunks)
}

func runCommitBlockOps(t *testing.T, store metadata.Store) {
	t.Helper()

	ctx := context.Background()

	makeHash := func(b byte) block.ContentHash {
		var h block.ContentHash
		h[0] = b
		return h
	}

	t.Run("FullCommit", func(t *testing.T) {
		rec := block.BlockRecord{
			BlockID:        "commit-full",
			BlockHash:      makeHash(0x01),
			Length:         2048,
			LiveChunkCount: 2,
			SyncState:      block.BlockStatePending,
		}
		chunks := []block.BlockChunkCommit{
			{
				Hash:   makeHash(0x10),
				Remote: block.ChunkLocator{BlockID: "commit-full", WireOffset: 0, WireLength: 1024},
			},
			{
				Hash:   makeHash(0x11),
				Remote: block.ChunkLocator{BlockID: "commit-full", WireOffset: 1024, WireLength: 1024},
			},
		}

		if err := store.CommitBlock(ctx, rec, chunks); err != nil {
			t.Fatalf("CommitBlock() error = %v", err)
		}

		// Block record persisted.
		got, found, err := store.GetBlockRecord(ctx, rec.BlockID)
		if err != nil {
			t.Fatalf("GetBlockRecord() error = %v", err)
		}
		if !found {
			t.Fatal("GetBlockRecord() found = false after CommitBlock")
		}
		if got != rec {
			t.Errorf("GetBlockRecord() = %+v, want %+v", got, rec)
		}

		// Remote locators synced.
		for i, c := range chunks {
			synced, err := store.IsSynced(ctx, c.Hash)
			if err != nil {
				t.Fatalf("IsSynced(chunk %d) error = %v", i, err)
			}
			if !synced {
				t.Errorf("IsSynced(chunk %d) = false, want true", i)
			}
			locator, found, err := store.GetLocator(ctx, c.Hash)
			if err != nil {
				t.Fatalf("GetLocator(chunk %d) error = %v", i, err)
			}
			if !found {
				t.Fatalf("GetLocator(chunk %d) found = false", i)
			}
			if locator != c.Remote {
				t.Errorf("GetLocator(chunk %d) = %+v, want %+v", i, locator, c.Remote)
			}
		}
	})

	t.Run("Idempotency", func(t *testing.T) {
		rec := block.BlockRecord{
			BlockID:        "commit-idem",
			BlockHash:      makeHash(0x02),
			Length:         512,
			LiveChunkCount: 3,
			SyncState:      block.BlockStatePending,
		}
		chunks := []block.BlockChunkCommit{
			{
				Hash:   makeHash(0x20),
				Remote: block.ChunkLocator{BlockID: "commit-idem", WireOffset: 0, WireLength: 512},
			},
		}

		if err := store.CommitBlock(ctx, rec, chunks); err != nil {
			t.Fatalf("CommitBlock() first call error = %v", err)
		}
		// Second call must be a no-op (not an error, not doubling count).
		if err := store.CommitBlock(ctx, rec, chunks); err != nil {
			t.Fatalf("CommitBlock() second call error = %v", err)
		}

		got, found, err := store.GetBlockRecord(ctx, rec.BlockID)
		if err != nil {
			t.Fatalf("GetBlockRecord() error = %v", err)
		}
		if !found {
			t.Fatal("GetBlockRecord() found = false")
		}
		// LiveChunkCount must still equal the first-call value (not doubled).
		if got.LiveChunkCount != rec.LiveChunkCount {
			t.Errorf("LiveChunkCount = %d after idempotent CommitBlock, want %d",
				got.LiveChunkCount, rec.LiveChunkCount)
		}
	})

	t.Run("Dedup", func(t *testing.T) {
		// Two chunks sharing the same content hash in one block must yield a
		// single synced entry (MarkSynced is idempotent).
		dupHash := makeHash(0x30)
		rec := block.BlockRecord{
			BlockID:        "commit-dedup",
			BlockHash:      makeHash(0x03),
			Length:         2048,
			LiveChunkCount: 1,
			SyncState:      block.BlockStatePending,
		}
		remote := block.ChunkLocator{BlockID: "commit-dedup", WireOffset: 0, WireLength: 1024}
		chunks := []block.BlockChunkCommit{
			{Hash: dupHash, Remote: remote},
			{Hash: dupHash, Remote: remote},
		}

		if err := store.CommitBlock(ctx, rec, chunks); err != nil {
			t.Fatalf("CommitBlock() error = %v", err)
		}

		// Exactly one synced entry exists for the shared hash.
		synced, err := store.IsSynced(ctx, dupHash)
		if err != nil {
			t.Fatalf("IsSynced() error = %v", err)
		}
		if !synced {
			t.Error("IsSynced() = false, want true for deduped chunk")
		}
		locator, found, err := store.GetLocator(ctx, dupHash)
		if err != nil {
			t.Fatalf("GetLocator() error = %v", err)
		}
		if !found {
			t.Fatal("GetLocator() found = false for deduped chunk")
		}
		if locator != remote {
			t.Errorf("GetLocator() = %+v, want %+v", locator, remote)
		}
	})

	t.Run("Atomicity", func(t *testing.T) {
		t.Run("SyncedLocatorFailureRollsBack", func(t *testing.T) {
			t.Parallel()

			rec := block.BlockRecord{
				BlockID:        "atomicity-retry",
				BlockHash:      makeHash(0xB0),
				Length:         512,
				LiveChunkCount: 1,
				SyncState:      block.BlockStatePending,
			}
			chunks := []block.BlockChunkCommit{
				{
					Hash:   makeHash(0xB1),
					Remote: block.ChunkLocator{BlockID: "atomicity-retry", WireOffset: 0, WireLength: 512},
				},
			}

			faulty := &faultySyncedStore{Store: store}

			// First call: the synced-marker write fails INSIDE the commit
			// transaction → the whole commit rolls back. Nothing may be
			// visible afterwards.
			err := faulty.CommitBlock(ctx, rec, chunks)
			require.Error(t, err, "first CommitBlock must fail on injected PutSyncedLocators failure")
			require.ErrorIs(t, err, errSyncedLocatorsInjected)

			_, found, err := store.GetBlockRecord(ctx, rec.BlockID)
			require.NoError(t, err)
			assert.False(t, found, "block record must not persist after synced-marker rollback")

			synced, err := store.IsSynced(ctx, chunks[0].Hash)
			require.NoError(t, err)
			assert.False(t, synced, "chunk must not be synced after synced-marker rollback")

			// Retry with no more faults: the full commit lands.
			err = faulty.CommitBlock(ctx, rec, chunks)
			require.NoError(t, err, "retry CommitBlock must succeed")

			got, found, err := store.GetBlockRecord(ctx, rec.BlockID)
			require.NoError(t, err)
			require.True(t, found, "block record must be present after retry")
			assert.Equal(t, rec.LiveChunkCount, got.LiveChunkCount,
				"LiveChunkCount must not be doubled by the retry")

			synced, err = store.IsSynced(ctx, chunks[0].Hash)
			require.NoError(t, err)
			assert.True(t, synced, "chunk must be marked synced after retry CommitBlock")

			locator, found, err := store.GetLocator(ctx, chunks[0].Hash)
			require.NoError(t, err)
			assert.True(t, found, "GetLocator must find chunk after retry")
			assert.Equal(t, chunks[0].Remote, locator)
		})
	})

	t.Run("LocatorOverwrite", func(t *testing.T) {
		// Chunks that already carry a DIFFERENT synced locator — the standalone
		// (zero-BlockID) form written by the legacy CAS mirror — must have it
		// OVERWRITTEN by the new block locator. This is the semantics the
		// cas→blocks migration relies on: MarkSynced alone is first-wins, but
		// CommitBlock's PutSyncedLocators is last-wins.
		h := makeHash(0x40)
		require.NoError(t, store.MarkSynced(ctx, h, block.ChunkLocator{}),
			"pre-seeding standalone locator")
		pre, found, err := store.GetLocator(ctx, h)
		require.NoError(t, err)
		require.True(t, found)
		require.True(t, pre.IsStandalone(), "pre-seeded locator must be standalone")

		rec := block.BlockRecord{
			BlockID:        "commit-overwrite",
			BlockHash:      makeHash(0x04),
			Length:         1024,
			LiveChunkCount: 1,
			SyncState:      block.BlockStateRemote,
		}
		remote := block.ChunkLocator{BlockID: "commit-overwrite", WireOffset: 0, WireLength: 1024}
		chunks := []block.BlockChunkCommit{
			{Hash: h, Remote: remote},
		}
		require.NoError(t, store.CommitBlock(ctx, rec, chunks))

		locator, found, err := store.GetLocator(ctx, h)
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, remote, locator,
			"CommitBlock must overwrite the standalone locator with the block locator")
	})

	t.Run("BatchedLocatorsMatchPerChunkWrites", func(t *testing.T) {
		// A commit carries every chunk's locator in one batched write. The rows
		// it leaves must be the ones a per-chunk delete-then-mark sequence would
		// leave: each hash resolves to its own locator, a pre-seeded standalone
		// marker is overwritten, and a hash repeated inside the batch ends on
		// the LAST locator given for it (the zero-chunk dedup that punch-hole
		// produces repeats a hash within one carve).
		hA, hB, hDup := makeHash(0x50), makeHash(0x51), makeHash(0x52)
		require.NoError(t, store.MarkSynced(ctx, hB, block.ChunkLocator{}),
			"pre-seeding standalone locator for the overwrite case")

		rec := block.BlockRecord{
			BlockID:        "commit-batched",
			BlockHash:      makeHash(0x05),
			Length:         4096,
			LiveChunkCount: 3,
			SyncState:      block.BlockStateRemote,
		}
		locA := block.ChunkLocator{BlockID: rec.BlockID, WireOffset: 0, WireLength: 1024}
		locB := block.ChunkLocator{BlockID: rec.BlockID, WireOffset: 1024, WireLength: 1024}
		dupFirst := block.ChunkLocator{BlockID: rec.BlockID, WireOffset: 2048, WireLength: 1024}
		dupLast := block.ChunkLocator{BlockID: rec.BlockID, WireOffset: 3072, WireLength: 1024}
		require.NoError(t, store.CommitBlock(ctx, rec, []block.BlockChunkCommit{
			{Hash: hA, Remote: locA},
			{Hash: hB, Remote: locB},
			{Hash: hDup, Remote: dupFirst},
			{Hash: hDup, Remote: dupLast},
		}))

		for _, want := range []struct {
			hash block.ContentHash
			loc  block.ChunkLocator
			name string
		}{
			{hA, locA, "fresh chunk"},
			{hB, locB, "overwritten standalone chunk"},
			{hDup, dupLast, "repeated chunk keeps its last locator"},
		} {
			got, found, err := store.GetLocator(ctx, want.hash)
			require.NoError(t, err)
			require.True(t, found, "%s must be synced after the batched commit", want.name)
			assert.Equal(t, want.loc, got, "%s", want.name)
		}
	})
}
