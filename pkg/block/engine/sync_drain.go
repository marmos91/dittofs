package engine

import (
	"context"
	"errors"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/chunker"
	"github.com/marmos91/dittofs/pkg/block/journal"
	"github.com/marmos91/dittofs/pkg/block/syncer"
)

// Flush quiesces a payload's local-side state and drains the pending-carve
// set: every locally stored chunk that has not yet been committed into a
// packed block is sealed, framed, uploaded via PutBlock, and atomically
// committed (record + locators + synced markers in one transaction, see
// DefaultCommitBlock). A crash between PutBlock and the commit leaves an
// orphan block object (reclaimed by GC) and re-carves the chunks into a new
// block — never losing or double-committing them.
//
// Return contract — see Store.Flush godoc for the full state
// machine and caller-retry guidance. In brief:
//   - Finalized=true, err=nil: the file's bytes are committed to their
//     sink — on a remote-backed share that is the remote; in local-only
//     mode it is the local tier plus the populated FileChunk manifest.
//   - Finalized=false, err=nil: SOFT condition (remote unhealthy, or the
//     flush substrate is not wired). Callers
//     MUST NOT tight-loop retry: surface the soft-fail to the protocol
//     adapter and let the client drive the next attempt on its own
//     schedule.
//   - err != nil: hard failure, do not retry until addressed.
//
// The carve drain serializes on carveMu against the background carve
// dispatcher, so an explicit Flush may block for the duration of an
// in-flight block build + PutBlock — bounded by one block (~16 MiB).
func (m *RemoteSync) Flush(ctx context.Context, payloadID string) (*block.FlushResult, error) {
	if err := m.checkReady(ctx); err != nil {
		return nil, err
	}

	// A remote without the carve substrate wired (partial test fixture) cannot
	// make anything durable: report the soft condition instead of claiming it.
	// Local-only mode (nil remote) still flushes: the flush populates the
	// FileChunk manifest (and File.Blocks) through the local-only sink, which
	// is what makes a local-only DrainRollups non-empty and clone/snapshot/
	// restore resolve the file's chunks. Only report the soft condition when
	// even the manifest substrate is missing.
	//
	// decision: this gate reads the wiring once and decides; flushFn takes its
	// own snapshot a few lines down, so the two are separate critical sections.
	// A setter landing between them makes the gate pass on a wired committer
	// and the pass run with a nil one, which commitManifestRows reports as a
	// hard error instead of the soft Finalized=false this gate documents. The
	// gate is per-read, not per-pass, and it is worth a wrong error class
	// because a nil committer still refuses to commit rather than claiming
	// rows it never wrote. The setters may run on a serving share by contract,
	// so the window is narrow rather than unreachable; no production caller
	// re-wires one today, which is the whole of why it has never been hit.
	// Fold the gate and the closure onto one snapshot the day one does.
	if m.remoteStore == nil {
		if _, _, committer, _ := m.wiring(); committer == nil {
			return &block.FlushResult{Finalized: false}, nil
		}
	} else if !m.IsRemoteHealthy() {
		// A down remote is the documented soft condition: leave the dirty
		// state untouched for the periodic uploader instead of surfacing every
		// PutObject 404/timeout as a hard wire error. The client re-drives on
		// its own schedule.
		return &block.FlushResult{Finalized: false}, nil
	}

	// Force-flush this file's dirty ranges into remote blocks and commit them
	// (the sink writes the FileChunk manifest rows in the same txn). The
	// journal serializes flush passes per shard, so the explicit drain and the
	// background dispatcher never pack the same range twice. fn + AfterFile
	// are built fresh per call (journal's C9 caller obligation).
	fn, reap := m.flushFn()
	if err := m.local.Flush(ctx, journal.FileID(payloadID), journal.FlushOptions{Force: true, AfterFile: reap}, fn); err != nil {
		return nil, err
	}
	return &block.FlushResult{Finalized: true}, nil
}

// DrainAllUploads performs an immediate synchronous upload of every local
// block to remote, bypassing the UploadDelay. Returns nil when every block
// reached remote, ctx.Err() on cancellation, or an aggregated error naming
// the blocks that failed to upload.
//
// Exposed via the REST API for the benchmark runner to call between test
// phases, and used by Close() to ensure no blocks are left stranded in the
// local store at shutdown.
func (m *RemoteSync) DrainAllUploads(ctx context.Context) error {
	if err := m.SyncNow(ctx); err != nil {
		return err
	}
	return ctx.Err()
}

// flushFn builds the fn + AfterFile pair one Flush pass calls back into: the
// carver assembly (fresh per call — the C9 caller obligation), the dedup Skip
// hook, the sink that seals/frames/uploads/commits, and the pass-end manifest
// reap (passed as FlushOptions.AfterFile, which journal calls under the
// shard's flush lock after the last flip). Built from the syncer's wired
// remote/committer/synced deps; the chunking profile is the engine's own
// config (the local store's seam is content-agnostic).
func (m *RemoteSync) flushFn() (journal.FlushFunc, func(context.Context, journal.FileID) error) {
	// The chunking profile is the engine's own config: the local store's flush
	// seam is content-agnostic and never held it. Invalid or unset degrades to
	// the historical default rather than failing the pass.
	params := m.config.ChunkParams
	if params.Validate() != nil {
		params = chunker.DefaultParams()
	}
	// One snapshot of the wired deps, taken under the lock the setters publish
	// them with — see wiring for why a field-at-a-time read is not equivalent.
	rbs, sealer, committer, hashStore := m.wiring()

	// A tier with no flush shape of its own answers 0 for both and the
	// defaults below stand in.
	window, blockSize := m.local.UploadConcurrency(), m.local.BlockSize()
	if window <= 0 {
		window = defaultBlockUploadWindow
	}
	if blockSize <= 0 {
		blockSize = paramsBlockSize(params)
	}
	// Both, not just the store: the two are published by different setters, so
	// (store wired, committer nil) is a legal snapshot even taken all at once,
	// and engineBlockSink dereferences the committer unguarded —
	// ManifestRowEndAfter and ReapSupersededManifest call straight through it
	// on any run with a durable tail. This is the same conjunction carveActive
	// is recomputed from. A store with no committer falls to the local-only
	// sink below, which fails the pass honestly instead of panicking.
	if rbs != nil && committer != nil {
		// The syncer's own uploadLimiter is the window, shared by every
		// concurrent pass rather than rebuilt per pass: blocks hold a slot from
		// submit until CommitBlock returns, so at most Limit() uploads (and
		// their arenas) are in flight across the whole syncer. A per-pass
		// semaphore here would nest inside the dispatcher's own window and make
		// the PUTs in flight their product, which is both a bound nobody
		// declared and a peak the controller cannot see.
		sink := engineBlockSink{sealer: sealer, rbs: rbs, committer: committer, commitLocks: &carveCommitLocks{}, onPutInFlight: m.notePutInFlight, onBlockUploaded: m.noteBlockUploaded, onBlockCommitted: m.noteBlockCommitted}
		// The dedup Skip hook consults the per-share synced-hash store: without
		// it every flush treats every chunk as novel and uploads whole new
		// blocks instead of landing manifest-only rows for content the remote
		// already holds.
		// The window must exist on this path. A nil semaphore is silently
		// accepted by the upload chain and bounds nothing, which is the same
		// failure shape as a window negotiated by type assertion that nothing
		// satisfies: no error, no bound, and no test notices. Every syncer
		// built by NewRemoteSync has one; this covers a hand-built struct.
		slots := m.uploadLimiter
		if slots == nil {
			slots = syncer.NewDynamicSemaphore(window)
		}
		return newFlushClosure(m.local, params, blockSize, engineDeduper{synced: hashStore}, sink, slots)
	}
	// Local-only (no remote block store): the flush cannot upload, but it must
	// still populate the FileChunk manifest (and project File.Blocks) so a
	// local-only DrainRollups is not a hard error and clone/snapshot/restore
	// resolve the file's chunks. The committer reaches here nil on more than
	// the clone fixture: SetSyncedHashStore clears it for any store that is
	// not a blockCommitter, and it may be handed one on an already-serving
	// share. That nil is not inert — CommitBlock fails the pass with "no
	// transactional committer wired" rather than reporting rows it never
	// wrote.
	//
	// This branch keeps a window of its own: uploadLimiter is an *upload*
	// window sized by a controller chasing uplink goodput, and there is no
	// uplink here to chase.
	sink := localBlockSink{committer: committer, commitLocks: &carveCommitLocks{}}
	return newFlushClosure(m.local, params, blockSize, localDeduper{}, sink, syncer.NewDynamicSemaphore(window))
}

// paramsBlockSize is the block-target fallback for a local store that exposes
// no BlockSize of its own (a test fixture): 256 average chunks per block, the
// same ratio the journal's historical CarveBlockSize default carried. A store
// with no chunking policy falls back to the historical 4 MiB default.
func paramsBlockSize(params chunker.Params) int64 {
	if params.Avg <= 0 {
		return 4 << 20
	}
	return int64(params.Avg) * 256
}

// SyncNow triggers an immediate flush drain of every locally stored chunk
// that has not yet been committed into a remote block. Blocks until the pass
// completes or the context is cancelled. Returns nil on full success,
// ctx.Err() on cancellation, or a wrapped error from the flush pass. Callers
// such as the REST /drain-uploads endpoint and Close() rely on this signal.
//
// Serializes against the background flush dispatcher via the shard's flush
// lock (inside journal.Flush), so the explicit drain never packs the same
// chunk twice.
func (m *RemoteSync) SyncNow(ctx context.Context) error {
	if m.remoteStore == nil {
		return nil
	}

	// decision: the same per-read gate as Flush's. carveActive can go false
	// between this check and the flushFn snapshot below, in which case the pass
	// commits nothing instead of returning the honest "not wired" error. Wrong
	// error class, not lost bytes — the sink still refuses the commit.
	if !m.carveActive.Load() {
		// A remote without the carve substrate cannot drain anything. Fail
		// honestly when dirty bytes are pending rather than claiming durability.
		if m.local.UnsyncedBytes() > 0 {
			return errors.New("syncer: carve substrate not wired — pending ranges cannot reach remote")
		}
		return nil
	}

	// fn + AfterFile are built fresh PER FILE (journal's C9 caller
	// obligation): the closure's carver and reap state carry across one
	// file's runs — hoisting them out of the loop interleaves two files'
	// bytes into one block and reaps file A's rows with file B's spans.
	var firstErr error
	for _, id := range m.local.ListFiles(ctx) {
		fn, reap := m.flushFn()
		if err := m.local.Flush(ctx, id, journal.FlushOptions{Force: true, AfterFile: reap}, fn); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// FlushAll force-flushes every file the local store indexes: one pass per
// file, so a whole-store drain the way SyncNow is an every-file drain. The
// empty FileID is NOT "all files" in journal.Flush — it is a single-file id
// like any other — so the drain must enumerate and Flush each id itself.
// fn + AfterFile are built fresh per file (journal's C9 caller obligation):
// the closure's carver and reap state carry across one file's runs, and a
// shared closure interleaves two files' bytes into one block.
func (m *RemoteSync) FlushAll(ctx context.Context) error {
	var firstErr error
	for _, id := range m.local.ListFiles(ctx) {
		res, err := m.Flush(ctx, string(id))
		if err == nil && res != nil && !res.Finalized {
			// A soft condition (unwired substrate) left this file unflushed;
			// reporting success would hide the dirty bytes from the caller.
			err = errors.New("syncer: flush not finalized for " + string(id))
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
