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
// same File row at once. Each commit re-projects File.Blocks (a SetManifest), so two
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

// manifestRows projects a carve batch into its per-file FileChunk rows. Data is
// nil for a deduped chunk, so the row length comes from Size.
func manifestRows(chunks []journal.CarveChunk) []*block.FileChunk {
	rows := make([]*block.FileChunk, 0, len(chunks))
	for i := range chunks {
		c := chunks[i]
		size := len(c.Data)
		if c.Data == nil {
			size = c.Size
		}
		rows = append(rows, &block.FileChunk{
			ID:       fmt.Sprintf("%s/%d", c.FileID, c.FileOffset),
			Hash:     block.ContentHash(c.Hash),
			DataSize: uint32(size),
			State:    block.BlockStatePending,
		})
	}
	return rows
}

// commitManifestRows writes a batch's manifest rows and re-materializes
// File.Blocks in one txn — the entire output of the local sink, and of a remote
// batch whose chunks all deduped. Merging only this batch's rows keeps a
// multi-batch carve from re-listing and re-sorting the whole growing manifest
// per batch; superseded rows are reaped once at run end.
func commitManifestRows(ctx context.Context, committer blockCommitter, locks *carveCommitLocks, payloadID string, rows []*block.FileChunk) error {
	if committer == nil {
		return fmt.Errorf("carve: no transactional committer wired")
	}
	// Serialize this file's commits so overlapping dispatcher calls don't abort
	// on the shared File-row projection under SSI.
	if mu := locks.forKey(payloadID); mu != nil {
		mu.Lock()
		defer mu.Unlock()
	}
	return committer.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return metadata.CommitCarvedChunks(ctx, tx, payloadID, rows)
	})
}

func (s localBlockSink) CommitBlock(ctx context.Context, chunks []journal.CarveChunk) error {
	if len(chunks) == 0 {
		return nil
	}
	return commitManifestRows(ctx, s.committer, s.commitLocks, string(chunks[0].FileID), manifestRows(chunks))
}

// ReapSupersededManifest implements journal's optional pass-end reap: once a
// carve pass's rows are all committed, delete the manifest rows they superseded so
// the per-file manifest tiles [0,size) with no stale straddler or gap (#953). A nil
// committer (the clone fixture) has no manifest to reap.
func (s localBlockSink) ReapSupersededManifest(ctx context.Context, id journal.FileID, spans [][2]int64, newOffsets map[int64]struct{}) error {
	if s.committer == nil {
		return nil
	}
	return reapSupersededManifest(ctx, s.committer, s.commitLocks, string(id), spans, newOffsets)
}

// ManifestRowEndAfter answers journal's run-extension query: how far the manifest
// coverage straddling off reaches, so a carve run does not stop inside a row it
// is about to supersede. A nil committer (the clone fixture) has no manifest, so
// the run stands as snapshotted.
func (s localBlockSink) ManifestRowEndAfter(ctx context.Context, id journal.FileID, off int64) (int64, error) {
	if s.committer == nil {
		return off, nil
	}
	return manifestRowEndAfter(ctx, s.committer, string(id), off)
}

// ManifestRowEndAfter answers journal's run-extension query for the
// remote-backed sink.
func (s engineBlockSink) ManifestRowEndAfter(ctx context.Context, id journal.FileID, off int64) (int64, error) {
	return manifestRowEndAfter(ctx, s.committer, string(id), off)
}

// manifestRowEndAfter runs the straddle lookup in a transaction, so it reads the
// same manifest the reap will mutate.
func manifestRowEndAfter(ctx context.Context, c blockCommitter, payloadID string, off int64) (int64, error) {
	end := off
	err := c.WithTransaction(ctx, func(tx metadata.Transaction) error {
		var err error
		end, err = metadata.ManifestRowEndAfter(ctx, tx, payloadID, off)
		return err
	})
	return end, err
}

// reapSupersededManifest runs the pass-end reap under the same per-file lock
// CommitBlock takes, since both end in a read-modify-write of the file's
// File.Blocks row and would otherwise abort each other under badger's SSI.
func reapSupersededManifest(ctx context.Context, c blockCommitter, locks *carveCommitLocks, payloadID string, spans [][2]int64, newOffsets map[int64]struct{}) error {
	if mu := locks.forKey(payloadID); mu != nil {
		mu.Lock()
		defer mu.Unlock()
	}
	return c.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return metadata.ReapSupersededManifest(ctx, tx, payloadID, spans, newOffsets)
	})
}

// ReapSupersededManifest implements journal's optional pass-end reap for the
// remote-backed sink: delete the manifest rows the carve pass superseded, atomic
// with a re-projection of File.Blocks (#953).
func (s engineBlockSink) ReapSupersededManifest(ctx context.Context, id journal.FileID, spans [][2]int64, newOffsets map[int64]struct{}) error {
	return reapSupersededManifest(ctx, s.committer, s.commitLocks, string(id), spans, newOffsets)
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
	// onBlockCommitted reports each block as it lands, carrying the block's
	// uploaded byte count. Reporting here rather than after a carve pass returns
	// is what makes the count advance *during* a long carve: the drain path
	// force-carves in one call that can run for many minutes, and its supervisor
	// reads these counters as a liveness signal. Nil in fixtures that don't care.
	onBlockCommitted func(bytes int64)
}

func (s engineBlockSink) CommitBlock(ctx context.Context, chunks []journal.CarveChunk) error {
	if len(chunks) == 0 {
		return nil
	}

	// Rows cover the whole batch; only chunks carrying bytes are framed and
	// uploaded. A deduped chunk is already remote-durable, so its row is all that
	// is missing — and it must land, or the run-end reap leaves the range with no
	// manifest coverage.
	fileChunks := manifestRows(chunks)
	var rawBytes int64
	novel := 0
	for i := range chunks {
		if chunks[i].Data != nil {
			novel++
			rawBytes += int64(len(chunks[i].Data))
		}
	}
	if novel == 0 {
		return commitManifestRows(ctx, s.committer, s.commitLocks, string(chunks[0].FileID), fileChunks)
	}

	blockID, err := newBlockID()
	if err != nil {
		return err
	}

	var buf bytes.Buffer
	// Pre-size so the block lands in one backing array: raw bytes plus per-chunk
	// codec/seal headroom. Best-effort — skipped on an absurd size rather than
	// risk a negative int conversion.
	if grow := rawBytes + int64(novel)*256 + 512; grow > 0 && grow <= math.MaxInt {
		buf.Grow(int(grow))
	}
	// nil header-sealer: bodies are sealed per-chunk below, matching the carver.
	builder, err := blockcodec.NewBuilder(&buf, blockID, nil)
	if err != nil {
		return fmt.Errorf("carve: new builder: %w", err)
	}

	commits := make([]block.BlockChunkCommit, 0, novel)
	for i := range chunks {
		c := chunks[i]
		if c.Data == nil {
			continue // deduped: manifest row only, nothing to frame
		}
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
	if s.onBlockCommitted != nil {
		s.onBlockCommitted(int64(len(blockBytes)))
	}
	return nil
}
