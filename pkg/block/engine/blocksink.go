package engine

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"sync"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/blockcodec"
	"github.com/marmos91/dittofs/pkg/block/journal"
	"github.com/marmos91/dittofs/pkg/block/remote"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// numCarveCommitStripes must be a power of two so the stripe index is a mask.
const numCarveCommitStripes = 256

// Compile-time guard: forKey masks with (numCarveCommitStripes-1), which only
// yields a valid index when the count is a power of two. If it is not, the
// unsigned subtraction below underflows and the build fails.
const _ = uint(-(numCarveCommitStripes & (numCarveCommitStripes - 1)))

// carveCommitLocks serializes a payloadID's metadata commit so the within-file
// carve dispatcher's concurrent CommitBlock calls do not read-modify-write the
// same File row at once. Each commit re-projects File.Blocks (a PutFile), so two
// overlapping commits for one file abort under badger's SSI as a transaction
// conflict; enough contention exhausts the retry budget and surfaces to the
// carver (the SMB/NFS client sees EDEADLK). The block upload runs OUTSIDE this
// lock, so overlapping successive blocks' uploads — the point of the concurrent
// dispatcher — is preserved. Distinct payloadIDs take distinct stripes and still
// commit concurrently; two that collide on a stripe serialize briefly, which is
// harmless (the commit transaction is short).
//
// A fixed stripe array bounds memory: a long-lived share carving many files
// never accumulates one mutex per file the way a keyed map would.
type carveCommitLocks struct {
	stripes [numCarveCommitStripes]sync.Mutex
}

// forKey returns the stripe mutex for payloadID, or nil when no stripes are
// wired (test fixtures that never exercise the concurrent dispatcher). A nil
// receiver makes the lock a no-op so those callers keep their prior behaviour.
func (c *carveCommitLocks) forKey(payloadID string) *sync.Mutex {
	if c == nil {
		return nil
	}
	// FNV-1a over the payloadID, masked to the stripe count (power of two).
	var h uint32 = 2166136261
	for i := 0; i < len(payloadID); i++ {
		h ^= uint32(payloadID[i])
		h *= 16777619
	}
	return &c.stripes[h&(numCarveCommitStripes-1)]
}

// engineDeduper answers journal's carve dedup oracle from the per-share
// synced-hash store: a chunk is durable once its hash has been mirrored to the
// remote at least once. A true result therefore means "remote-durable", the
// contract journal.Deduper requires before a record's synced bit may flip.
type engineDeduper struct {
	synced metadata.SyncedHashStore
}

func (d engineDeduper) IsChunkDurable(ctx context.Context, hash journal.ChunkHash) (bool, error) {
	return d.synced.IsSynced(ctx, block.ContentHash(hash))
}

// localDeduper is the carve dedup oracle for a share with NO remote block store.
// There is nothing to be "remote-durable" against, so every chunk is treated as
// novel — carve packs it and localBlockSink records its FileChunk manifest row.
type localDeduper struct{}

func (localDeduper) IsChunkDurable(context.Context, journal.ChunkHash) (bool, error) {
	return false, nil
}

// localBlockSink is the BlockSink for a remote-less (local-only) share. The
// journal owns the bytes durably on local disk, so carve neither frames a block
// nor uploads (no PutBlock) — it only records the per-file FileChunk manifest
// rows (hash + DataSize, no remote block key). Those rows are what clone reads
// (O(1) reflink of the ChunkRef list) and what snapshot/restore project into
// FileAttr.Blocks; without them a local-only DrainRollups could not populate the
// manifest at all (the whole point of the local carve path).
//
// Rows + the File.Blocks projection are written in one txn via the committer
// (the per-share metadata store, wired unconditionally as SyncedHashStore). The
// clone fixture has no committer, but its source has no dirty data so CommitBlock
// never fires — a nil committer there is inert.
type localBlockSink struct {
	committer   blockCommitter
	commitLocks *carveCommitLocks
}

func (s localBlockSink) CommitBlock(ctx context.Context, chunks []journal.CarveChunk) error {
	if len(chunks) == 0 {
		return nil
	}
	if s.committer == nil {
		return fmt.Errorf("local carve: no transactional committer wired")
	}
	payloadID := string(chunks[0].FileID)
	fileChunks := make([]*block.FileChunk, 0, len(chunks))
	for i := range chunks {
		c := chunks[i]
		fileChunks = append(fileChunks, &block.FileChunk{
			ID:       fmt.Sprintf("%s/%d", c.FileID, c.FileOffset),
			Hash:     block.ContentHash(c.Hash),
			DataSize: uint32(len(c.Data)),
			State:    block.BlockStatePending,
		})
	}
	// Serialize this file's commits so overlapping dispatcher calls don't abort
	// on the shared File-row projection under SSI.
	if mu := s.commitLocks.forKey(payloadID); mu != nil {
		mu.Lock()
		defer mu.Unlock()
	}
	return s.committer.WithTransaction(ctx, func(tx metadata.Transaction) error {
		for _, fc := range fileChunks {
			if err := tx.Put(ctx, fc); err != nil {
				return fmt.Errorf("local carve: put manifest row %s: %w", fc.ID, err)
			}
		}
		// Materialize File.Blocks from the manifest — same txn (R). Superseded-row
		// reaping runs once at run end (ReapSupersededManifest), not per batch.
		return metadata.ProjectManifestToBlocks(ctx, tx, payloadID)
	})
}

// ReapSupersededManifest implements journal's optional run-end reap: once a carve
// run's rows are all committed, delete the manifest rows the run superseded so the
// per-file manifest tiles [0,size) with no stale straddler or gap (#953). A nil
// committer (the clone fixture) has no manifest to reap.
func (s localBlockSink) ReapSupersededManifest(ctx context.Context, id journal.FileID, runStart, runEnd int64, newOffsets map[int64]struct{}) error {
	if s.committer == nil {
		return nil
	}
	return s.committer.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return metadata.ReapSupersededManifest(ctx, tx, string(id), runStart, runEnd, newOffsets)
	})
}

// ReapSupersededManifest implements journal's optional run-end reap for the
// remote-backed sink: delete the manifest rows the carve run superseded, atomic
// with a re-projection of File.Blocks (#953).
func (s engineBlockSink) ReapSupersededManifest(ctx context.Context, id journal.FileID, runStart, runEnd int64, newOffsets map[int64]struct{}) error {
	return s.committer.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return metadata.ReapSupersededManifest(ctx, tx, string(id), runStart, runEnd, newOffsets)
	})
}

// engineBlockSink is journal's production BlockSink: it seals each carved chunk,
// frames them into one block via blockcodec, uploads the block with PutBlock,
// and atomically commits the block record + synced locators + per-file manifest
// rows. It mirrors Syncer.carveAndCommitBlock minus the local-byte resolution —
// journal hands the plaintext in-hand on each CarveChunk.
type engineBlockSink struct {
	sealer      remote.ChunkSealer
	rbs         remote.RemoteBlockStore
	committer   blockCommitter
	commitLocks *carveCommitLocks
}

func (s engineBlockSink) CommitBlock(ctx context.Context, chunks []journal.CarveChunk) error {
	if len(chunks) == 0 {
		return nil
	}

	blockID, err := newBlockID()
	if err != nil {
		return err
	}

	var rawBytes int64
	for i := range chunks {
		rawBytes += int64(len(chunks[i].Data))
	}
	var buf bytes.Buffer
	// Pre-size so the block lands in one backing array: raw bytes plus per-chunk
	// codec/seal headroom. Best-effort — skipped on an absurd size rather than
	// risk a negative int conversion.
	if grow := rawBytes + int64(len(chunks))*256 + 512; grow > 0 && grow <= math.MaxInt {
		buf.Grow(int(grow))
	}
	// nil header-sealer: bodies are sealed per-chunk below, matching the carver.
	builder, err := blockcodec.NewBuilder(&buf, blockID, nil)
	if err != nil {
		return fmt.Errorf("carve: new builder: %w", err)
	}

	commits := make([]block.BlockChunkCommit, 0, len(chunks))
	fileChunks := make([]*block.FileChunk, 0, len(chunks))
	for i := range chunks {
		c := chunks[i]
		h := block.ContentHash(c.Hash)

		wire := c.Data
		if s.sealer != nil {
			wire, err = s.sealer.SealChunk(ctx, h, c.Data)
			if err != nil {
				return fmt.Errorf("carve: seal chunk %s: %w", h, err)
			}
		}
		chunkLoc, err := builder.Add(h, wire)
		if err != nil {
			return fmt.Errorf("carve: frame chunk %s: %w", h, err)
		}
		// Local stays zero — the journal owns the local bytes, so there is no
		// log-blob location to record (DefaultCommitBlock treats zero as "none").
		commits = append(commits, block.BlockChunkCommit{Hash: h, Remote: chunkLoc})
		fileChunks = append(fileChunks, &block.FileChunk{
			ID:       fmt.Sprintf("%s/%d", c.FileID, c.FileOffset),
			Hash:     h,
			DataSize: uint32(len(c.Data)),
			State:    block.BlockStatePending,
		})
	}
	if _, err := builder.Finish(); err != nil {
		return fmt.Errorf("carve: finish block: %w", err)
	}

	blockBytes := buf.Bytes()
	blockHash := block.ContentHash(blake3.Sum256(blockBytes))

	// PutBlock first: a crash before the commit leaves an orphan block (GC
	// reclaims it), never an unbacked record.
	if err := s.rbs.PutBlock(ctx, blockID, bytes.NewReader(blockBytes)); err != nil {
		return fmt.Errorf("carve: put block %s: %w", blockID, err)
	}

	rec := block.BlockRecord{
		BlockID:        blockID,
		BlockHash:      blockHash,
		Length:         int64(len(blockBytes)),
		LiveChunkCount: uint32(len(commits)),
		SyncState:      block.BlockStateRemote,
	}
	// Only the metadata commit is serialized per file (the shared File-row
	// projection under SSI); the PutBlock upload above ran concurrently with the
	// next block's, which is the whole point of the overlapping dispatcher.
	commit := func() error {
		if mu := s.commitLocks.forKey(string(chunks[0].FileID)); mu != nil {
			mu.Lock()
			defer mu.Unlock()
		}
		return metadata.DefaultCommitBlock(ctx, s.committer, rec, commits, fileChunks)
	}
	if err := commit(); err != nil {
		return fmt.Errorf("carve: commit block %s: %w", blockID, err)
	}
	return nil
}
