package journal

import (
	"context"
	"errors"
	"time"
)

// ErrLocalStoreFull is returned by the write path when the local cache is at its
// MaxLocalBytes cap and no segment can be evicted because every one is pinned by
// unsynced (dirty) records. The writer must retry after carve drains those bytes
// to the remote store; UnsyncedBytes reports how much is pinned.
var ErrLocalStoreFull = errors.New("journal: local store full, all segments pinned by unsynced bytes")

// evictBackoff is the poll interval the write path waits between eviction
// attempts while dirty-pinned. The tunable knobs are Config.MaxLocalBytes (the
// threshold) and Config.EvictMaxWait (the give-up budget); this fixed step just
// paces the wait and is not worth a config field.
const evictBackoff = 10 * time.Millisecond

// EvictResult reports what an eviction pass reclaimed. Held distinguishes the
// two ways a pass reclaims nothing: the gate was closed (Held), or the gate was
// open and no segment qualified. Both are ordinary, non-error outcomes, so a
// caller reporting "freed 0" to an operator has no other way to tell a
// suspended store from one whose every segment is pinned by unsynced records.
type EvictResult struct {
	SegmentsEvicted int
	BytesFreed      int64
	Held            bool
}

// Evict frees whole sealed segments under storage pressure, coldest first
// (approx-LRU by lastAccess). Only sealed, fully-synced segments qualify: a
// segment holding any unsynced record is skipped so eviction never destroys the
// only copy of dirty bytes (the synced-gate, mirroring logblob.EvictBlob). It
// evicts until targetBytes have been freed; targetBytes <= 0 evicts a single
// qualifying segment. It returns what it reclaimed — SegmentsEvicted == 0 means
// nothing qualified.
//
// Evict is the explicit force-evict entrypoint (the shares evict admin's
// DrainLocalSynced). When the sealed set cannot satisfy the reclaim it force-seals
// the fully-synced active segments so their bytes become evictable too: a working
// set smaller than the segment-roll threshold otherwise sits entirely in the
// never-sealed active segment, where nothing can reclaim it.
func (s *Store) Evict(ctx context.Context, targetBytes int64) (EvictResult, error) {
	return s.evict(ctx, targetBytes)
}

// evict is the shared eviction loop. Its force-seal fall-through is bounded to
// one pass per call, and sealableActive keeps it from touching an active
// holding unsynced records.
func (s *Store) evict(ctx context.Context, targetBytes int64) (EvictResult, error) {
	if err := ctx.Err(); err != nil {
		return EvictResult{}, err
	}
	if s.closed.Load() {
		return EvictResult{}, errClosed
	}
	if s.evictionHeld() {
		return EvictResult{Held: true}, nil
	}
	var res EvictResult
	sealedActives := false
	for {
		seg, sh := s.claimColdestEvictable()
		if seg == nil {
			// Sealed set exhausted. Seal the fully-synced active segments once so
			// their bytes become evictable — the next iteration drains them.
			// Bounded to a single pass so a fresh (empty) active segment is never
			// sealed in a spin.
			if !sealedActives {
				sealedActives = true
				sealed, err := s.sealSyncedActives(ctx)
				if err != nil {
					return res, err
				}
				if sealed {
					continue
				}
			}
			return res, nil
		}
		freed, err := s.evictSegment(sh, seg)
		if err != nil {
			return res, err
		}
		res.SegmentsEvicted++
		res.BytesFreed += freed
		if targetBytes <= 0 || res.BytesFreed >= targetBytes {
			return res, nil
		}
	}
}

// sealableActive reports whether act can be force-sealed: it holds at least one
// record, every record is synced (remote-durable), no other pass has claimed it,
// and no live snapshot pins its bytes. records raises monotonically and
// syncedRecords only rises toward it, so the fully-synced check is stable while
// the caller holds the shard lock.
func (s *Store) sealableActive(act *segmentMeta) bool {
	return act != nil && act.records.Load() > 0 &&
		act.syncedRecords.Load() == act.records.Load() &&
		!act.busy.Load() && !s.pinned(act)
}

// sealSyncedActives force-seals each shard's active segment when it holds only
// synced (remote-durable) records, moving it into the sealed set so eviction can
// reclaim it. It never seals an empty active (nothing to gain, and a spin would
// spew empty segments), an active holding any unsynced record (sealing would not
// make it evictable and its dirty bytes must stay the local copy), or a pinned
// active (a live snapshot needs those bytes local). A seal here is the same
// primitive as a rotation, so it produces an identically valid sealed footer.
// It returns whether it sealed any segment and surfaces a seal failure (fsync or
// next-segment creation) rather than masking it as a no-op, and stops early if
// ctx is cancelled. Caller holds no lock.
//
// ponytail: the shard lock is held across the seal's four fsyncs (two in
// sealInPlace, two more creating the replacement segment), stalling that one
// shard's appends, reads and commits for their duration. Eviction and repack
// snapshot-then-unlock instead, but only because their victim is sealed —
// immutable, unreachable by any appender — and their busy claim merely excludes
// each other; the append path never consults that claim, so it does not transfer
// to an active segment. Unlocking here would let a record land between the data
// fsync and the sealed bit, which is the ordering the seal exists to guarantee.
// Excluding appends some other way is the upgrade path, worth it only if a drain
// stall shows up in a profile — this runs once per operator-triggered drain, one
// shard at a time, and the same seal already runs under the same lock on every
// segment rotation in the write path.
func (s *Store) sealSyncedActives(ctx context.Context) (bool, error) {
	sealedAny := false
	for _, sh := range s.shards {
		if err := ctx.Err(); err != nil {
			return sealedAny, err
		}
		sh.mu.Lock()
		if s.sealableActive(sh.active) {
			err := s.sealSegment(sh)
			sh.mu.Unlock()
			if err != nil {
				return sealedAny, err
			}
			sealedAny = true
			continue
		}
		sh.mu.Unlock()
	}
	return sealedAny, nil
}

// claimColdestEvictable finds the coldest sealed, fully-synced, unclaimed segment
// across all shards and claims it (busy CAS) so eviction and GC never race on the
// same segment. It returns nil only when no segment qualifies. Losing the claim
// to a concurrent GC/evict does not abort the pass: that segment is now busy and
// drops out of the next scan, so the coldest remaining candidate is tried
// instead. The retry terminates because a lost CAS means the segment is busy,
// shrinking the candidate set each round.
func (s *Store) claimColdestEvictable() (*segmentMeta, *shard) {
	for {
		var (
			best       *segmentMeta
			bestShard  *shard
			bestAccess int64
		)
		for _, sh := range s.shards {
			sh.mu.Lock()
			for _, seg := range sh.sealed {
				if !evictable(seg) {
					continue
				}
				if s.pinned(seg) {
					// Pinned by a live snapshot: its bytes are the only durable copy
					// of an at-or-below-watermark record (local-only) — never evict.
					continue
				}
				if la := seg.evictionAge(); best == nil || la < bestAccess {
					best, bestShard, bestAccess = seg, sh, la
				}
			}
			sh.mu.Unlock()
		}
		if best == nil {
			return nil, nil
		}
		// The synced-gate only ever loosens for a sealed segment (carve raises
		// syncedRecords toward records; records is frozen once sealed), so the sole
		// concurrency hazard is another claimer — the CAS settles it.
		if best.busy.CompareAndSwap(false, true) {
			return best, bestShard
		}
	}
}

// evictSegment removes one claimed sealed segment. Every interval-tree entry
// backed by the segment becomes a cold marker — so a later read of that range
// fetches from the remote store instead of seeing a false POSIX hole, and
// DataExtents still reports the range as present — and only then is the segment
// retired. Caller must have claimed seg.busy.
//
// The markers are persisted to the cold log FIRST, while the segment is still on
// disk: they are the only remaining record that those ranges hold data, so a
// crash between marking and unlinking must not be able to lose them. Persisting
// first can at worst leave a marker for bytes that are still local (the eviction
// did not complete), which costs a needless remote fetch — the opposite order
// would serve zeros.
//
// A failure drops the claim (as reclaimEmptied does) so a later pass can retry the
// segment: the claim is exclusive, so keeping it would bar the segment from every
// subsequent eviction and GC pass, stranding its bytes under the very disk pressure
// eviction serves. Retrying is safe because the failure leaves the index and the
// segment untouched; at worst it left markers for bytes that are still local, which
// the persist-first order already tolerates.
//
// flushMu is held for the same reason reclaimEmptied and the GC pass hold it, and
// the segment-busy claim does not cover: a carve pass flips its records synced as
// each block commits, but the manifest rows those fresh rows superseded are reaped
// only once it has packed the whole file. A record that flipped in that window is
// evictable while stale rows still overlap its range, and coverage resolves to the
// greatest covering start — so cold-marking it there lets a stale row starting
// later than a fresh one serve old bytes on the cold read. Holding flushMu also
// keeps the segment's fd open under carve's unlocked preads, which readRecord
// already documents as relying on it.
//
// ponytail: this waits out the shard's whole carve pass, uploads included, rather
// than skipping the shard; make it a TryLock that moves on to the next shard's
// coldest segment if a writer is ever seen stalling behind a live carve.
func (s *Store) evictSegment(sh *shard, seg *segmentMeta) (freed int64, err error) {
	sh.flushMu.Lock()
	defer sh.flushMu.Unlock()

	defer func() {
		if err != nil {
			seg.busy.Store(false)
		}
	}()

	sh.mu.Lock()
	var entries []coldEntry
	for id, fi := range sh.index {
		for k := range fi.ivs {
			// A non-cold interval of this segment is cold-marked even when a
			// partial overwrite re-marked its surviving fragment dirty in place:
			// every record of a sealed segment was committed remotely (the
			// record counter is fully synced), so the fragment's bytes are the
			// remote store's copy and the marker is what recovers them. Skipping
			// the fragment here would leave it pointing at bytes retireSegment
			// unlinks below — reads would fail instead of hydrating.
			iv := &fi.ivs[k]
			if iv.loc.SegmentID == seg.id && !iv.cold {
				entries = append(entries, coldEntry{
					id:      id,
					fileOff: iv.fileOff,
					length:  iv.length,
					version: iv.version,
					// Copied off the interval being replaced, so it dates the content.
					provenance: coldFromData,
				})
			}
		}
	}
	sh.mu.Unlock()

	// The files this segment backs are exactly the ones the scan produced an
	// entry for, deduplicated.
	backed := make(map[FileID]struct{}, len(entries))
	for _, e := range entries {
		backed[e.id] = struct{}{}
	}

	// This append's fsync is load-bearing and must stay per call: the bytes it
	// describes are unlinked below, so the log is about to be their only record.
	// demote is the residency-loss chokepoint: a failed append leaves the
	// intervals resident and returns ErrStateLost instead of evicting blind.
	if err = s.demote(entries, nil); err != nil {
		// Without a durable marker the range would come back from a restart as a
		// hole, so keep the segment (and its bytes) instead of evicting blind.
		return 0, err
	}

	// Flip only the files the scan above found backed by this segment, rather
	// than walking the shard index a second time. No file can join that set in
	// between: the segment is sealed (nothing appends to it) and claimed, so no
	// repack can repoint an interval into it either. A file that LEFT the set —
	// its interval superseded while the cold log was being written — simply has
	// nothing left to flip.
	//
	// ponytail: one full index walk remains, in the scan above. Removing it needs
	// a segment→intervals reverse index, worth building only if eviction shows up
	// in a profile.
	sh.mu.Lock()
	for id := range backed {
		fi := sh.index[id]
		if fi == nil {
			continue
		}
		for k := range fi.ivs {
			if fi.ivs[k].loc.SegmentID == seg.id && !fi.ivs[k].cold {
				fi.ivs[k].cold = true
			}
		}
	}
	sh.mu.Unlock()
	return s.retireSegment(sh, seg)
}

// ensureSpace is the write-path capacity gate. With MaxLocalBytes set, it evicts
// cold synced segments to fit the incoming write; when nothing is evictable
// because every segment is dirty-pinned, it backpressures the writer up to
// EvictMaxWait (giving carve time to drain to the remote) and finally returns
// ErrLocalStoreFull. A no-op when MaxLocalBytes is unset. Holds no lock, so the
// eviction it drives never contends with the caller's own shard.
//
// MaxLocalBytes is a soft pressure threshold, not a hard byte quota: admission
// reads diskBytes without reserving, so N concurrent writers across shards can
// each clear the gate and append before any lands, briefly overshooting the cap
// — and eviction is whole-segment anyway, so exact enforcement is neither
// possible nor the goal. The gate relieves pressure (evict) and, failing that,
// backpressures; a later writer's round evicts the overshoot. ErrLocalStoreFull
// means genuinely nothing is evictable, never mere overshoot. A hard ceiling
// would need a global reserved-bytes counter, deliberately not built (the
// eviction design is lazy and pressure-gated).
func (s *Store) ensureSpace(ctx context.Context, needed int64) error {
	if s.cfg.MaxLocalBytes <= 0 {
		return nil
	}
	// The backpressure tail below is reachable on every share, not just
	// remote-backed ones: eviction only ever reclaims a segment whose records
	// are all synced, because dropping the last copy of a byte would lose it.
	// A share with nothing offloaded therefore has nothing evictable, so
	// hitting MaxLocalBytes lands straight in the stall-then-ErrLocalStoreFull
	// path.
	deadline := time.Now().Add(s.cfg.EvictMaxWait)
	lastUnsynced := s.unsynced.Load()
	warned := false
	for s.diskBytes.Load()+needed > s.cfg.MaxLocalBytes {
		if err := ctx.Err(); err != nil {
			return err
		}
		overage := s.diskBytes.Load() + needed - s.cfg.MaxLocalBytes
		// evict's force-seal fall-through is what makes the cap reachable at all:
		// sealing otherwise happens only when an append would overflow
		// SegmentSize, so a working set below that threshold sits entirely in
		// active segments, and eviction scans the sealed set alone — every byte
		// synced and droppable, nothing evictable. It cannot strand this writer's
		// own dirty bytes: sealableActive refuses any active still holding an
		// unsynced record, so a sustained writer's segment stays put and
		// backpressures.
		res, err := s.evict(ctx, overage)
		if err != nil {
			return err
		}
		if res.SegmentsEvicted > 0 {
			deadline = time.Now().Add(s.cfg.EvictMaxWait) // reclaimed: extend the budget
			lastUnsynced = s.unsynced.Load()
			continue
		}
		// Nothing evictable. As long as carve keeps draining unsynced bytes to the
		// remote, backpressure (slow) rather than error: refresh the budget on every
		// observed drain so a writer that merely outpaces a live syncer waits for it
		// instead of failing. ErrLocalStoreFull stays reserved for a genuine stall —
		// the cap is pinned by unsynced bytes and no drain progress happens for the
		// whole EvictMaxWait budget (no syncer, or sync disabled) — so the wait is
		// bounded and never hangs.
		if cur := s.unsynced.Load(); cur < lastUnsynced {
			lastUnsynced = cur
			deadline = time.Now().Add(s.cfg.EvictMaxWait)
		}
		if !warned {
			// The fields are the discriminator, so state the observation and let
			// them name the cause: unsynced_bytes>0 means carve is behind and the
			// wait will clear; either eviction flag means it never will, and they
			// are reported apart because the operator's next move differs —
			// suspended is a remote outage to fix, pinned is a retention policy to
			// change. All zero and false leaves a snapshot pinning every candidate.
			s.log.Warn("journal local store full: nothing evictable, backpressuring writes",
				"dir", s.dir,
				"disk_bytes", s.diskBytes.Load(),
				"max_local_bytes", s.cfg.MaxLocalBytes,
				"unsynced_bytes", s.unsynced.Load(),
				"eviction_suspended", s.evictionSuspended.Load(),
				"eviction_pinned", s.evictionPinned.Load())
			warned = true
		}
		if time.Now().After(deadline) {
			return ErrLocalStoreFull
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(evictBackoff):
		}
	}
	return nil
}
