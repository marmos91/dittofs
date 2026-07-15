package engine

import (
	"bytes"
	"context"
	"fmt"
	"math"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/blockcodec"
	"github.com/marmos91/dittofs/pkg/block/journal"
	"github.com/marmos91/dittofs/pkg/block/remote"
	"github.com/marmos91/dittofs/pkg/metadata"
)

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

// engineBlockSink is journal's production BlockSink: it seals each carved chunk,
// frames them into one block via blockcodec, uploads the block with PutBlock,
// and atomically commits the block record + synced locators + per-file manifest
// rows. It mirrors Syncer.carveAndCommitBlock minus the local-byte resolution —
// journal hands the plaintext in-hand on each CarveChunk.
type engineBlockSink struct {
	sealer    remote.ChunkSealer
	rbs       remote.RemoteBlockStore
	committer blockCommitter
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
	if err := metadata.DefaultCommitBlock(ctx, s.committer, rec, commits, fileChunks); err != nil {
		return fmt.Errorf("carve: commit block %s: %w", blockID, err)
	}
	return nil
}
