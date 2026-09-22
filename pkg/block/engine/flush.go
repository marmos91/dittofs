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
	"github.com/marmos91/dittofs/pkg/block/carver"
	"github.com/marmos91/dittofs/pkg/block/gc"
	"github.com/marmos91/dittofs/pkg/block/journal"
	"github.com/marmos91/dittofs/pkg/block/remote"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// The flush-side collaborators the journal seam calls back into. They live
// here, next to their only consumer (the fn closure flushPass builds), because
// every one of them touches pkg/block, blockcodec or the metadata store — none
// of which the journal knows.

// ChunkHash is the BLAKE3-256 content hash of a chunk's plaintext, keyed so
// the deduper and the sink agree on identical bytes.
type ChunkHash = carver.Hash

// Deduper reports whether a chunk is already durable on the remote store. A
// true result MUST mean "remote-durable", never merely "seen locally", or a
// flip could clean bytes that never reached remote. Production wiring backs
// this with the per-share synced-hash oracle.
type Deduper interface {
	IsChunkDurable(ctx context.Context, hash ChunkHash) (bool, error)
}

// CarveChunk is one content-defined chunk handed to the sink for packing.
type CarveChunk struct {
	Hash       ChunkHash
	FileID     journal.FileID
	FileOffset int64  // logical offset of the chunk within the file
	Size       int    // chunk length; authoritative when Data is nil
	Data       []byte // plaintext; nil when the chunk deduped (nothing to upload)
}

// BlockSink seals, frames, uploads (PutBlock) and atomically commits one
// block's worth of novel chunks. CommitBlock is atomic: a non-nil error means
// nothing became durable, so the caller leaves the covered fragments dirty.
//
// Lifetime contract: CarveChunk.Data slices are backed by the carver's
// per-block arena, which covers only the blocks in flight. An implementation
// MUST NOT retain any Data slice after CommitBlock returns; copy the bytes
// first if it needs them longer.
type BlockSink interface {
	CommitBlock(ctx context.Context, chunks []CarveChunk) error
}

// SupersededReaper is an optional BlockSink capability. Once a flush pass has
// committed a file's rows, journal calls ReapSupersededManifest from AfterFile
// so the sink can delete the manifest rows they superseded — keeping the
// per-file FileChunk manifest a gap-free, overlap-free tiling of [0,size)
// after a partial overwrite. spans are the committed parts of the pass's
// re-carved (dirty) runs, disjoint and ascending; newOffsets are the chunk
// offsets the pass wrote (so the reap keeps them and deletes only stale
// straddlers/interior rows). One call per file rather than one per run: the
// sink re-reads the whole manifest to answer it. Sinks without a metadata
// store (test fakes) simply don't implement it and the reap is skipped.
type SupersededReaper interface {
	ReapSupersededManifest(ctx context.Context, id journal.FileID, spans [][2]int64, newOffsets map[int64]struct{}) error
}

// ManifestRowEnder is an optional BlockSink capability: it reports how far the
// manifest coverage straddling an offset reaches. The flush closure uses it to
// widen a run to a row boundary before packing it, so the fresh tiling covers
// every row the run-end reap deletes. Sinks without a metadata store (test
// fakes) don't implement it: a run is packed exactly as offered.
//
// The answer is the greatest end among rows starting strictly before off and
// reaching past it, or off itself when none does — never a value below off.
type ManifestRowEnder interface {
	ManifestRowEndAfter(ctx context.Context, id journal.FileID, off int64) (int64, error)
}

// ClobberGuard is an optional BlockSink capability, and it exists because a
// manifest row is keyed by the file offset of its first claimed byte while the
// commit that writes a row is an upsert. A run starting exactly on an existing
// row's offset therefore REPLACES that row rather than superseding it: the row
// is gone before the run-end reap ever lists the manifest, and everything it
// claimed past the run's end is left with no cover at all.
//
// The closure calls PreserveClobberedRow once per run, before the run is
// packed and so while the row still exists, naming the run's final bounds and
// the ranges past its end that are still OWED. The sink re-keys whatever the
// row about to be replaced still owns, restricted to those ranges.
//
// owed is what makes this safe: the manifest alone cannot tell a range that
// lost its cover from a range that is SUPPOSED to have none. A punched hole
// must read as zeros, and re-covering it with the replaced row's pre-punch
// content is its own corruption. journal answers from the interval index,
// which distinguishes them: owed carries only ranges durable on the remote
// (evicted or resident), and excludes both holes and ranges still dirty for a
// later pass.
type ClobberGuard interface {
	PreserveClobberedRow(ctx context.Context, id journal.FileID, runStart, runEnd int64, owed [][2]int64) error
}

// carveCommitLocks serializes a payloadID's metadata commit so the
// within-file dispatcher's overlapping commits do not read-modify-write the
// same File.Blocks row at once (badger SSI aborts on that). The block upload
// runs OUTSIDE this lock, so overlapping successive blocks' uploads — the
// point of the concurrent dispatcher — is preserved. A fixed stripe array
// bounds memory: a long-lived share flushing many files never accumulates one
// mutex per file the way a keyed map would.
const numCarveCommitStripes = 256

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

// engineDeduper answers the flush dedup oracle from the per-share synced-hash
// store: a chunk is durable once its hash has been mirrored to the remote at
// least once. A true result therefore means "remote-durable", the contract
// before a record's synced bit may flip.
type engineDeduper struct {
	synced metadata.SyncedHashStore
}

// IsChunkDurable answers through gc.AdoptDedup, which records the adoption so a
// remote sweep running concurrently cannot reclaim the hash the carver is
// about to point a manifest row at.
func (d engineDeduper) IsChunkDurable(ctx context.Context, hash ChunkHash) (bool, error) {
	h := block.ContentHash(hash)
	return gc.AdoptDedup(h, func() (bool, error) {
		return d.synced.IsSynced(ctx, h)
	})
}

// localDeduper is the dedup oracle for a share with NO remote block store.
// There is nothing to be "remote-durable" against, so every chunk is treated
// as novel — the carver packs it and localBlockSink records its FileChunk
// manifest row.
type localDeduper struct{}

func (localDeduper) IsChunkDurable(context.Context, ChunkHash) (bool, error) {
	return false, nil
}

// The sinks implement BlockSink plus all three optional capabilities, declared
// here so a signature change on either side fails the build instead of
// silently skipping the reap, the row-widen and the clobber guard at runtime.
var (
	_ BlockSink        = localBlockSink{}
	_ SupersededReaper = localBlockSink{}
	_ ManifestRowEnder = localBlockSink{}
	_ ClobberGuard     = localBlockSink{}

	_ BlockSink        = engineBlockSink{}
	_ SupersededReaper = engineBlockSink{}
	_ ManifestRowEnder = engineBlockSink{}
	_ ClobberGuard     = engineBlockSink{}
)

// localBlockSink is the sink for a remote-less (local-only) share. The journal
// owns the bytes durably on local disk, so nothing frames a block or uploads
// (no PutBlock) — it only records the per-file FileChunk manifest rows (hash +
// DataSize, no remote block key). Those rows are what clone reads (O(1)
// reflink of the ChunkRef list) and what snapshot/restore project into
// FileAttr.Blocks; without them a local-only DrainRollups could not populate
// the manifest at all.
//
// Rows + the File.Blocks projection are written in one txn via the committer.
// The committer may be nil — SetSyncedHashStore clears it for any store that
// is not a blockCommitter, and the clone fixture never wires one — and that
// nil is not inert. The row-end and reap queries below read it as "no manifest
// to consult" and stand down, but CommitBlock fails the pass rather than
// report rows it never wrote.
type localBlockSink struct {
	committer   blockCommitter
	commitLocks *carveCommitLocks
}

// manifestRows projects a flush batch into its per-file FileChunk rows. Data
// is nil for a deduped chunk, so the row length comes from Size.
func manifestRows(chunks []CarveChunk) []*block.FileChunk {
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
// File.Blocks in one txn. Merging only this batch's rows keeps a multi-batch
// flush from re-listing and re-sorting the whole growing manifest per batch;
// superseded rows are reaped once at run end.
func commitManifestRows(ctx context.Context, committer blockCommitter, locks *carveCommitLocks, payloadID string, rows []*block.FileChunk) error {
	if committer == nil {
		return fmt.Errorf("flush: no transactional committer wired")
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

func (s localBlockSink) CommitBlock(ctx context.Context, chunks []CarveChunk) error {
	if len(chunks) == 0 {
		return nil
	}
	return commitManifestRows(ctx, s.committer, s.commitLocks, string(chunks[0].FileID), manifestRows(chunks))
}

// preserveClobberedRow implements the optional clobber guard: before a run
// whose first fresh chunk lands on an existing row's key replaces that row,
// keep whatever it still owns past the run's end, over the ranges journal
// reports as still owed. A nil committer (the clone fixture) has no manifest
// to keep.
func preserveClobberedRow(
	ctx context.Context,
	committer blockCommitter,
	locks *carveCommitLocks,
	payloadID string,
	runStart, runEnd int64,
	owed [][2]int64,
) error {
	if committer == nil {
		return nil
	}
	// Same File-row serialization as the commit path: this writes manifest rows
	// and re-projects File.Blocks, so it races the same way under SSI.
	if mu := locks.forKey(payloadID); mu != nil {
		mu.Lock()
		defer mu.Unlock()
	}
	return committer.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return metadata.PreserveClobberedRow(ctx, tx, payloadID, runStart, runEnd, owed)
	})
}

func (s localBlockSink) PreserveClobberedRow(ctx context.Context, id journal.FileID, runStart, runEnd int64, owed [][2]int64) error {
	return preserveClobberedRow(ctx, s.committer, s.commitLocks, string(id), runStart, runEnd, owed)
}

func (s engineBlockSink) PreserveClobberedRow(ctx context.Context, id journal.FileID, runStart, runEnd int64, owed [][2]int64) error {
	return preserveClobberedRow(ctx, s.committer, s.commitLocks, string(id), runStart, runEnd, owed)
}

// ReapSupersededManifest implements the optional pass-end reap: once a flush
// pass's rows are all committed, delete the manifest rows they superseded so
// the per-file manifest tiles [0,size) with no stale straddler or gap. A nil
// committer (the clone fixture) has no manifest to reap.
func (s localBlockSink) ReapSupersededManifest(ctx context.Context, id journal.FileID, spans [][2]int64, newOffsets map[int64]struct{}) error {
	if s.committer == nil {
		return nil
	}
	return reapSupersededManifest(ctx, s.committer, s.commitLocks, string(id), spans, newOffsets)
}

// ManifestRowEndAfter answers the run-extension query: how far the manifest
// coverage straddling off reaches, so a run does not stop inside a row it is
// about to supersede. A nil committer (the clone fixture) has no manifest, so
// the run stands as offered.
func (s localBlockSink) ManifestRowEndAfter(ctx context.Context, id journal.FileID, off int64) (int64, error) {
	if s.committer == nil {
		return off, nil
	}
	return manifestRowEndAfter(ctx, s.committer, string(id), off)
}

// ManifestRowEndAfter answers the run-extension query for the remote-backed
// sink.
func (s engineBlockSink) ManifestRowEndAfter(ctx context.Context, id journal.FileID, off int64) (int64, error) {
	return manifestRowEndAfter(ctx, s.committer, string(id), off)
}

// manifestRowEndAfter runs the straddle lookup in a transaction, so it reads
// the same manifest the reap will mutate.
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

// ReapSupersededManifest implements the optional pass-end reap for the
// remote-backed sink: delete the manifest rows the flush pass superseded,
// atomic with a re-projection of File.Blocks.
func (s engineBlockSink) ReapSupersededManifest(ctx context.Context, id journal.FileID, spans [][2]int64, newOffsets map[int64]struct{}) error {
	return reapSupersededManifest(ctx, s.committer, s.commitLocks, string(id), spans, newOffsets)
}

// engineBlockSink is the production sink: it seals each carved chunk, frames
// them into one block via blockcodec, uploads the block with PutBlock, and
// atomically commits the block record + synced locators + per-file manifest
// rows.
type engineBlockSink struct {
	sealer      remote.ChunkSealer
	rbs         remote.RemoteBlockStore
	committer   blockCommitter
	commitLocks *carveCommitLocks
	// onPutInFlight brackets the PutBlock call itself: +1 before, -1 after it
	// returns either way. It is what lets the controller sample upload
	// concurrency without the metadata commit folded in. Nil in fixtures that
	// don't care.
	onPutInFlight func(delta int64)
	// onBlockUploaded reports each block's bytes the moment PutBlock returns,
	// before the metadata commit. That is the signal the upload window is
	// actually steering: the commit is serialized per file and no amount of
	// upload concurrency relieves it, so folding its latency into the goodput
	// sample would have the controller shrink the window in answer to a
	// bottleneck somewhere else entirely. Nil in fixtures that don't care.
	onBlockUploaded func(bytes int64)
	// onBlockCommitted reports each block as it lands durably, after the
	// commit. Reporting here rather than after a flush pass returns is what
	// makes the count advance *during* a long flush: the drain path
	// force-flushes in one call that can run for many minutes, and its
	// supervisor reads this counter as a liveness signal. Nil in fixtures
	// that don't care.
	onBlockCommitted func(bytes int64)
}

func (s engineBlockSink) CommitBlock(ctx context.Context, chunks []CarveChunk) error {
	if len(chunks) == 0 {
		return nil
	}

	// Rows cover the whole batch; only chunks carrying bytes are framed and
	// uploaded. A deduped chunk is already remote-durable, so its row is all
	// that is missing — and it must land, or the run-end reap leaves the range
	// with no manifest coverage.
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

	blockID, err := block.NewBlockID()
	if err != nil {
		return fmt.Errorf("carve: %w", err)
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
		return fmt.Errorf("flush: new builder: %w", err)
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
			wire, err = s.sealer.SealChunk(ctx, h, wire)
			if err != nil {
				return fmt.Errorf("flush: seal chunk %s: %w", h, err)
			}
		}
		loc, err := builder.Add(h, wire)
		if err != nil {
			return fmt.Errorf("flush: append chunk %s: %w", h, err)
		}
		commits = append(commits, block.BlockChunkCommit{Hash: h, Remote: loc})
	}
	if _, err := builder.Finish(); err != nil {
		return fmt.Errorf("flush: finish block: %w", err)
	}

	blockBytes := buf.Bytes()
	blockHash := block.ContentHash(blake3.Sum256(blockBytes))

	// PutBlock first: a crash before the commit leaves an orphan block (GC
	// reclaims it), never an unbacked record. The upload slot is held by the
	// upload chain (acquired before this goroutine spawned), and that window is
	// shared by every carve pass, so concurrent blocks never exceed it
	// syncer-wide rather than merely per pass.
	if s.onPutInFlight != nil {
		s.onPutInFlight(1)
	}
	err = s.rbs.PutBlock(ctx, blockID, bytes.NewReader(blockBytes))
	if s.onPutInFlight != nil {
		s.onPutInFlight(-1)
	}
	if err != nil {
		return fmt.Errorf("flush: put block %s: %w", blockID, err)
	}
	if s.onBlockUploaded != nil {
		s.onBlockUploaded(int64(len(blockBytes)))
	}

	rec := block.BlockRecord{
		BlockID:        blockID,
		BlockHash:      blockHash,
		Length:         int64(len(blockBytes)),
		LiveChunkCount: uint32(len(commits)),
		SyncState:      block.BlockStateRemote,
	}
	// Only the metadata commit is serialized per file (the shared File-row
	// projection under SSI); the PutBlock upload above ran concurrently with
	// the next block's, which is the whole point of the overlapping
	// dispatcher.
	if err := s.commit(ctx, string(chunks[0].FileID), rec, commits, fileChunks); err != nil {
		return fmt.Errorf("flush: commit block %s: %w", blockID, err)
	}
	if s.onBlockCommitted != nil {
		s.onBlockCommitted(int64(len(blockBytes)))
	}
	return nil
}

// commit writes one block's record + locators + manifest rows, serialized per
// file on the shared File-row projection under SSI.
func (s engineBlockSink) commit(ctx context.Context, payloadID string, rec block.BlockRecord, commits []block.BlockChunkCommit, fileChunks []*block.FileChunk) error {
	if mu := s.commitLocks.forKey(payloadID); mu != nil {
		mu.Lock()
		defer mu.Unlock()
	}
	return metadata.DefaultCommitBlock(ctx, s.committer, rec, commits, fileChunks)
}

// Flush ensures all dirty data for a payload is persisted by delegating
// to the syncer's flush drain. CAS StoreChunk already dedups physically
// by content hash, so no separate file-level dedup hook runs here.
//
// Flush is the single COMMIT/CLOSE seam for every protocol (NFSv3 COMMIT,
// NFSv4 COMMIT, NFS DATA_SYNC/FILE_SYNC WRITE, SMB Flush/CLOSE all reach it
// via common.CommitBlockStore). The WRITE path honors NFS UNSTABLE and defers
// the append-log fsync, so this is where that fsync is paid: SyncPayload
// makes the payload's page-cache-resident records durable BEFORE the syncer
// drain and BEFORE we report success. A fsync failure aborts the flush so the
// durability point never falsely acks.
//
// Return-value contract:
//
//   - (Finalized=true, nil)
//     All locally-mirrored data for payloadID is durable on the
//     configured remote. Callers may report COMMIT/Flush success
//     to the client.
//
//   - (Finalized=false, nil)
//     A NON-fatal soft condition prevented finalization THIS call:
//     no remote is configured (local-only mode — local quiesce
//     completed but no remote durability target exists), the remote
//     is configured but currently unhealthy, or another in-flight
//     mirror pass (periodic uploader or overlapping Flush) is
//     already running. The dirty state is unchanged and will be
//     re-attempted on the next Flush or the next periodic uploader
//     tick.
//
//     Callers driving NFS COMMIT or SMB Flush loops MUST rate-limit
//     their retries against this branch. A tight retry storm on
//     Finalized=false starves the uploading goroutine (the
//     CompareAndSwap gate in syncer.Flush makes the explicit caller
//     LOSE every retry attempt against the periodic uploader's
//     in-flight tick) and pegs the CPU without making progress.
//     Recommended pattern: surface the soft-fail to the protocol
//     adapter and let the client drive the next attempt on its own
//     schedule (e.g. NFSv3 reports the WRITE's "committed" enum as
//     UNSTABLE rather than DATASYNC/FILESYNC so the client reissues
//     COMMIT later; SMB Flush returns success after a bounded
//     attempt) rather than spin in-handler.
//
//   - (nil, err)
//     Hard failure (I/O error, remote.Put rejection, MarkSynced
//     metadata error). Do NOT retry until the underlying condition
//     is addressed; the caller should surface a protocol-level
//     error to the client.
func (bs *Store) Flush(ctx context.Context, payloadID string) (*block.FlushResult, error) {
	if err := bs.enter(); err != nil {
		return nil, err
	}
	defer bs.closeMu.RUnlock()
	// Durability barrier: fsync the deferred writes first.
	if err := bs.local.Commit(ctx, journal.FileID(payloadID)); err != nil {
		return nil, err
	}
	// A durable local store already makes the payload crash-safe at this point,
	// so under the default (async-remote) policy the ack must NOT block on the
	// remote mirror — that is exactly what common.CommitBlockStore documents.
	// Flushing to the remote synchronously here turned every FILE_SYNC/DATA_SYNC
	// WRITE into an inline S3 PutObject the reply waited on: multi-second per-op
	// stalls at ~3% CPU. The background flush loop mirrors the data; a strict
	// share (require_durable_commit) still drains inline below.
	//
	// Still perform the per-payload FileChunk metadata quiesce that syncer.Flush
	// would (persist queued manifest updates so reads and restart-recovery see
	// the authoritative manifest) — only the remote flush drain is skipped.
	if bs.LocalDurable() && !bs.RequireDurableCommit() {
		return &block.FlushResult{Finalized: false}, nil
	}
	// Delegate to the syncer's flush drain.
	return bs.syncer.Flush(ctx, payloadID)
}

// DrainAllUploads forces every dirty payload through the flush drain and then
// waits for all pending remote uploads to complete.
//
// The force-flush must run first: it is what turns still-dirty journal data
// into CAS chunks, which is the only thing the flush packs to the remote.
// Draining the syncer alone leaves any data still inside the journal
// un-chunked, so it never reaches the remote and the caller's durability
// guarantee silently does not hold. The snapshot path rolls up explicitly
// before calling this; the standalone `system drain-uploads` path relies on
// the flush here.
func (bs *Store) DrainAllUploads(ctx context.Context) error {
	if err := bs.enter(); err != nil {
		return err
	}
	defer bs.closeMu.RUnlock()
	// Force-flush every dirty range to the remote (bypassing the age/size
	// batching gate), then wait for the uploads to settle.
	if err := bs.syncer.FlushAll(ctx); err != nil {
		return err
	}
	return bs.syncer.DrainAllUploads(ctx)
}

// SyncCounts returns the lifetime (completed, failed) sync counts for this
// store: chunks that reached the remote and failed flush upload attempts.
// Both are monotonic. The drain-uploads idle watchdog reads them as a
// progress signal. Returns (0, 0) when the store is closing or has no remote
// (local-only stores never sync, so the counters are meaningless — matching
// stats.go, which also reports zeros in that mode).
func (bs *Store) SyncCounts() (completed, failed int) {
	if err := bs.enter(); err != nil {
		return 0, 0
	}
	defer bs.closeMu.RUnlock()
	if bs.remote == nil {
		return 0, 0
	}
	return bs.syncer.SyncCounts()
}

// DrainRollups forces the local store to flush every currently-dirty payload
// into CAS + the FileChunk manifest, bypassing the batching gate. The
// snapshot-create orchestration calls this BEFORE the metadata Backup() so
// the dump observes a fully-populated FileAttr.Blocks (and therefore a
// non-empty snapshot manifest). It must run before DrainAllUploads — the
// flush is what produces the CAS chunks that then pack to the remote.
func (bs *Store) DrainRollups(ctx context.Context) error {
	if err := bs.enter(); err != nil {
		return err
	}
	defer bs.closeMu.RUnlock()
	return bs.syncer.FlushAll(ctx)
}

// ColdSeed is one payload's worth of work for SeedColdBatch: the payload ID and
// the {offset, length} extents of it to mark cold.
type ColdSeed struct {
	PayloadID string
	Extents   [][2]int64
}

// SeedColdBatch marks the seeds' extents as remote-durable-but-not-local so a
// subsequent read faults them in from the remote store rather than zero-filling.
// Snapshot restore and the pre-journal upgrade both use it (remote-backed shares
// only) to arm cold reads over a local tier that holds none of the bytes.
//
// Callers pass many payloads at once because the local tier makes the markers
// durable once per call, not once per payload — an fsync per file is nearly all
// of a manifest seed's wall clock on a share with many small files. Every entry
// is held in memory until that write, so callers bound their own batches.
//
// The batch is one lifecycle-gated op, so a Close waits out the append in flight
// and the seeds after it fail fast rather than reopening the cold log behind a
// torn-down store. Seeding in batches rather than one call for a whole manifest
// is what keeps that wait short.
//
// A tier that cannot hold a range it does not have records nothing, which the
// caller only hits on non-remote paths anyway.
func (bs *Store) SeedColdBatch(ctx context.Context, seeds []ColdSeed) error {
	if err := bs.enter(); err != nil {
		return err
	}
	defer bs.closeMu.RUnlock()
	js := make([]journal.ColdSeed, 0, len(seeds))
	for _, sd := range seeds {
		js = append(js, journal.ColdSeed{ID: journal.FileID(sd.PayloadID), Extents: sd.Extents})
	}
	return bs.local.SeedColdBatch(ctx, js)
}

// SeedColdRefs gives the local tier an account of the ranges a payload's chunk
// refs place, marking each one remote-durable-but-not-local. A server-side copy
// hands the destination the source's refs without moving a byte, so the
// destination's ranges exist in the manifest and nowhere in the index; this is
// what puts them there, and it is the caller's post-commit step — seeding before
// the manifest lands would describe rows a rolled-back copy never wrote.
//
// It changes no read: a hole on a remote-backed share already reconciles against
// the manifest and hydrates exactly as a cold range does. What it changes is
// everything that reasons about residency from the index — the remote-only byte
// count an operator acts on, and OfflineReadiness, which can tell a copy nobody
// has read back from a range whose interval was lost only once the copy has one.
//
// Only the parts of each ref the index does not already describe are seeded, so
// a copy over a destination that still holds local bytes leaves those alone, and
// a repeat call costs nothing. Refs with no hash place no bytes (a sparse hole
// carries no chunk) and are skipped, as are empty ones.
//
// One file at a time, under its shard lock, because the destination is live —
// see journal.Store.SeedCold.
func (bs *Store) SeedColdRefs(ctx context.Context, payloadID string, refs []block.ChunkRef) error {
	if err := bs.enter(); err != nil {
		return err
	}
	defer bs.closeMu.RUnlock()
	// Nothing to record on a share with no remote: a cold range there has
	// nowhere to hydrate from and fails its reads closed, where the same range
	// left as a hole reads as the zeros it is. Local-only copies materialize real
	// bytes into the destination's own journal, which describes them already, so
	// there is no work here rather than work being refused.
	if !bs.HasRemoteStore() {
		return nil
	}
	extents := make([][2]int64, 0, len(refs))
	for _, r := range refs {
		if r.Size == 0 || r.Hash.IsZero() {
			continue
		}
		// The index addresses its ranges in int64, so a ref out past that could
		// only be described as a negative offset. Skipping it leaves the range a
		// hole, which still reconciles against the manifest on read.
		if r.Offset > math.MaxInt64-uint64(r.Size) {
			continue
		}
		extents = append(extents, [2]int64{int64(r.Offset), int64(r.Size)})
	}
	if len(extents) == 0 {
		return nil
	}
	return bs.local.SeedCold(ctx, journal.FileID(payloadID), extents)
}

// DiscardLocalContent drops the local tier's whole account of a payload, so a
// read of it resolves against the manifest instead of against bytes the tier
// still holds.
//
// It is the local half of replacing a payload's content wholesale. A
// server-side copy rewrites the destination's manifest and moves no byte, and
// every range the destination still holds locally then describes content the
// copy replaced. A covered warm range reports neither hole nor cold, so the
// read never reaches the new manifest: the destination serves its pre-copy
// content indefinitely, with nothing logged.
//
// What it leaves behind is holes, which is the state a range with no local copy
// is supposed to be in and the state a fresh destination is already in: a hole
// the manifest covers hydrates from the remote, and one it does not reads as
// the zeros a sparse range is. Marking the span cold instead would fail the
// sparse case closed, because an absent range and a stale one are not the same
// state and only the manifest can tell them apart.
//
// The local tier makes the clip durable before it takes effect and fences it by
// version, so a write that raced past it survives and a crash cannot resurrect
// what it dropped.
//
// The caller owns the ordering: this runs after the copy's metadata transaction
// commits, never before. Until that commit lands, the bytes it drops are the
// destination's real content, and a rolled-back copy could not get them back.
func (bs *Store) DiscardLocalContent(ctx context.Context, payloadID string) error {
	if err := bs.enter(); err != nil {
		return err
	}
	defer bs.closeMu.RUnlock()
	if err := bs.local.Truncate(ctx, journal.FileID(payloadID), 0); err != nil {
		return err
	}
	// The read cache is keyed by content hash, so it cannot serve the dropped
	// bytes for content that no longer addresses them. Its per-payload
	// sequential tracker is keyed by payload, though, and would have prefetch
	// chasing the hashes the payload held before — the same reset every other
	// path that changes a payload's content underneath the tier does.
	bs.loadCache().OnRead(payloadID, nil, 0)
	return nil
}

// RestoreToVersion rewinds the local journal to a snapshot's version watermark
// and re-materializes that point-in-time view durably at the log head. It is the
// local-only snapshot-restore primitive the runtime calls instead of
// ResetLocalState when the share has no remote store (the local tier is the
// only durable copy of the bytes).
func (bs *Store) RestoreToVersion(ctx context.Context, v uint64) error {
	if err := bs.enter(); err != nil {
		return err
	}
	defer bs.closeMu.RUnlock()
	return bs.local.RestoreToVersion(ctx, v)
}

// SetPinVersion sets the local tier's snapshot pin watermark so GC/eviction
// keep the bytes of every at-or-below-watermark record (the durable copy for a
// live local-only snapshot).
func (bs *Store) SetPinVersion(v uint64) { bs.local.SetPinVersion(v) }

// JournalVersion returns the local tier's current LSN watermark, captured by
// snapshot create (after DrainRollups) to record the snapshot's version.
func (bs *Store) JournalVersion() uint64 { return bs.local.JournalVersion() }

// ResetLocalState drops every file's locally cached ranges so post-restore reads
// resolve purely through the restored manifest. The snapshot-restore
// orchestration calls it BEFORE the metadata Reset() + Restore(), not after:
// clearing the local tier first leaves no dirty interval for a background rollup
// worker to flush into the freshly-restored metadata, so a file modified in place
// after the snapshot is never served from a stale local record overlaid on the
// restored bytes.
func (bs *Store) ResetLocalState(ctx context.Context) error {
	if err := bs.enter(); err != nil {
		return err
	}
	defer bs.closeMu.RUnlock()
	// Drop every file's local cached ranges so post-restore reads resolve
	// purely through the restored manifest + remote (there is no append-log
	// overlay to clear anymore — the journal IS the local tier).
	for _, payloadID := range bs.local.ListFiles(ctx) {
		if err := bs.local.Delete(ctx, journal.FileID(payloadID)); err != nil {
			return err
		}
	}
	return nil
}
