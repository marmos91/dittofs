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

// errPutLocalInjected is the sentinel returned by faultyLocalLocationStore.
var errPutLocalInjected = errors.New("injected PutLocalLocation failure")

// faultyLocalLocationStore wraps a Store and makes PutLocalLocation fail
// inside WithTransaction. CommitBlock delegates to metadata.DefaultCommitBlock
// with itself as the receiver so the injected WithTransaction is exercised.
type faultyLocalLocationStore struct {
	metadata.Store
	errPut error
}

func (f *faultyLocalLocationStore) WithTransaction(ctx context.Context, fn func(metadata.Transaction) error) error {
	return f.Store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return fn(&faultyLocalLocationTx{Transaction: tx, errPut: f.errPut})
	})
}

func (f *faultyLocalLocationStore) CommitBlock(ctx context.Context, rec block.BlockRecord, chunks []block.BlockChunkCommit) error {
	return metadata.DefaultCommitBlock(ctx, f, rec, chunks)
}

type faultyLocalLocationTx struct {
	metadata.Transaction
	errPut error
}

func (tx *faultyLocalLocationTx) PutLocalLocation(ctx context.Context, hash block.ContentHash, loc block.LocalChunkLocation) error {
	if tx.errPut != nil {
		return tx.errPut
	}
	return tx.Transaction.PutLocalLocation(ctx, hash, loc)
}

// errMarkSyncedInjected is the sentinel returned by faultyMarkSyncedStore.
var errMarkSyncedInjected = errors.New("injected MarkSynced failure")

// faultyMarkSyncedStore wraps a Store and makes MarkSynced fail the first time
// it is called, then delegates subsequent calls. CommitBlock delegates to
// metadata.DefaultCommitBlock with itself as the receiver so the injected
// MarkSynced is actually exercised.
type faultyMarkSyncedStore struct {
	metadata.Store
	mu        sync.Mutex
	hasFailed bool
}

func (f *faultyMarkSyncedStore) MarkSynced(ctx context.Context, hash block.ContentHash, loc block.ChunkLocator) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.hasFailed {
		f.hasFailed = true
		return errMarkSyncedInjected
	}
	return f.Store.MarkSynced(ctx, hash, loc)
}

func (f *faultyMarkSyncedStore) CommitBlock(ctx context.Context, rec block.BlockRecord, chunks []block.BlockChunkCommit) error {
	return metadata.DefaultCommitBlock(ctx, f, rec, chunks)
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
				Local:  block.LocalChunkLocation{LogBlobID: "log-001", RawOffset: 0, RawLength: 1024},
			},
			{
				Hash:   makeHash(0x11),
				Remote: block.ChunkLocator{BlockID: "commit-full", WireOffset: 1024, WireLength: 1024},
				Local:  block.LocalChunkLocation{LogBlobID: "log-001", RawOffset: 1024, RawLength: 1024},
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

		// Local locations persisted.
		for i, c := range chunks {
			loc, found, err := store.GetLocalLocation(ctx, c.Hash)
			if err != nil {
				t.Fatalf("GetLocalLocation(chunk %d) error = %v", i, err)
			}
			if !found {
				t.Fatalf("GetLocalLocation(chunk %d) found = false", i)
			}
			if loc != c.Local {
				t.Errorf("GetLocalLocation(chunk %d) = %+v, want %+v", i, loc, c.Local)
			}
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
				Local:  block.LocalChunkLocation{LogBlobID: "log-002", RawOffset: 0, RawLength: 512},
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
		local := block.LocalChunkLocation{LogBlobID: "log-003", RawOffset: 0, RawLength: 1024}
		chunks := []block.BlockChunkCommit{
			{Hash: dupHash, Remote: remote, Local: local},
			{Hash: dupHash, Remote: remote, Local: local},
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

		// And the single local location resolves correctly.
		loc, found, err := store.GetLocalLocation(ctx, dupHash)
		if err != nil {
			t.Fatalf("GetLocalLocation() error = %v", err)
		}
		if !found {
			t.Fatal("GetLocalLocation() found = false for deduped chunk")
		}
		if loc != local {
			t.Errorf("GetLocalLocation() = %+v, want %+v", loc, local)
		}
	})

	t.Run("Atomicity", func(t *testing.T) {
		t.Run("InTxRollback", func(t *testing.T) {
			t.Parallel()

			rec := block.BlockRecord{
				BlockID:        "atomicity-rollback",
				BlockHash:      makeHash(0xA0),
				Length:         1024,
				LiveChunkCount: 1,
				SyncState:      block.BlockStatePending,
			}
			chunks := []block.BlockChunkCommit{
				{
					Hash:   makeHash(0xA1),
					Remote: block.ChunkLocator{BlockID: "atomicity-rollback", WireOffset: 0, WireLength: 1024},
					Local:  block.LocalChunkLocation{LogBlobID: "log-atm-01", RawOffset: 0, RawLength: 1024},
				},
			}

			faulty := &faultyLocalLocationStore{Store: store, errPut: errPutLocalInjected}
			err := faulty.CommitBlock(ctx, rec, chunks)
			require.Error(t, err, "CommitBlock must fail on injected PutLocalLocation error")
			require.ErrorIs(t, err, errPutLocalInjected)

			// Neither block record nor local location must have persisted: tx rolled back.
			_, found, err := store.GetBlockRecord(ctx, rec.BlockID)
			require.NoError(t, err)
			assert.False(t, found, "block record must not persist after in-tx rollback")

			_, found, err = store.GetLocalLocation(ctx, chunks[0].Hash)
			require.NoError(t, err)
			assert.False(t, found, "local location must not persist after in-tx rollback")
		})

		t.Run("CrossPhaseRetry", func(t *testing.T) {
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
					Local:  block.LocalChunkLocation{LogBlobID: "log-atm-02", RawOffset: 0, RawLength: 512},
				},
			}

			faulty := &faultyMarkSyncedStore{Store: store}

			// First call: transaction commits (block record + local location written)
			// but MarkSynced fails → error returned. This mimics a crash between the
			// commit phase and the MarkSynced phase.
			err := faulty.CommitBlock(ctx, rec, chunks)
			require.Error(t, err, "first CommitBlock must fail on injected MarkSynced failure")
			require.ErrorIs(t, err, errMarkSyncedInjected)

			// Retry with no more MarkSynced faults. The idempotency guard must NOT
			// short-circuit before the MarkSynced loop — all remote locators must be
			// written even though the block record already exists from the first call.
			err = faulty.CommitBlock(ctx, rec, chunks)
			require.NoError(t, err, "retry CommitBlock must succeed")

			// Block record must be present with the original LiveChunkCount (not doubled).
			got, found, err := store.GetBlockRecord(ctx, rec.BlockID)
			require.NoError(t, err)
			require.True(t, found, "block record must be present after retry")
			assert.Equal(t, rec.LiveChunkCount, got.LiveChunkCount,
				"LiveChunkCount must not be doubled by the retry")

			// Local location must be present.
			lloc, found, err := store.GetLocalLocation(ctx, chunks[0].Hash)
			require.NoError(t, err)
			require.True(t, found, "local location must be present after retry")
			assert.Equal(t, chunks[0].Local, lloc)

			// Remote locator must now be marked synced.
			synced, err := store.IsSynced(ctx, chunks[0].Hash)
			require.NoError(t, err)
			assert.True(t, synced, "chunk must be marked synced after retry CommitBlock")

			locator, found, err := store.GetLocator(ctx, chunks[0].Hash)
			require.NoError(t, err)
			assert.True(t, found, "GetLocator must find chunk after retry")
			assert.Equal(t, chunks[0].Remote, locator)
		})
	})
}
