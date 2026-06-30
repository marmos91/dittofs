package storetest

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// CommitBlockProvider is implemented by stores with full CommitBlock support.
// The conformance suite type-asserts to this and skips if unimplemented.
type CommitBlockProvider interface {
	CommitBlockEnabled() bool
}

func runCommitBlockOps(t *testing.T, store metadata.Store) {
	t.Helper()
	if p, ok := store.(CommitBlockProvider); !ok || !p.CommitBlockEnabled() {
		t.Skip("CommitBlock not implemented by this backend")
	}

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
}
