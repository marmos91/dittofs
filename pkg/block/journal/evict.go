package journal

import (
	"context"
	"errors"
	"fmt"
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
	return s.evict(ctx, targetBytes, false)
}

// evict is the shared eviction loop. Its force-seal fall-through is bounded to
// one pass per call, and sealableActive keeps it from touching an active
// holding unsynced records.
//
// pressure says the caller is the write-path capacity gate rather than the
// operator drain, and only sets whether the reclaim is metered — see the
// observation below.
func (s *Store) evict(ctx context.Context, targetBytes int64, pressure bool) (EvictResult, error) {
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
			if errors.Is(err, errTornRecord) {
				// carryMarkersForward quarantined it: its bytes stay on disk and
				// evictable now refuses it, so the next claim picks a different
				// segment instead of this one forever. Aborting here would hand
				// the error to ensureSpace and turn one damaged segment into a
				// store-wide write outage.
				continue
			}
			return res, err
		}
		res.SegmentsEvicted++
		res.BytesFreed += freed
		// decision: only a reclaim the capacity gate drove is counted. Three other
		// callers reach a segment unlink — repackSegment and dropVictim (repack.go),
		// reclaimEmptied (reclaim.go) — and so does this loop under Evict, the
		// operator drain. None of them says anything about disk pressure: a repack
		// relocates bytes and keeps them warm, a post-delete reclaim frees bytes
		// nobody holds, and the drain reclaims the whole resident set of an idle
		// store on request. Counting any of them puts a step into evictions_total
		// on a store that was never short of space, which is what a rate() alert
		// on it reads as pressure. The cost is that a drain's reclaim is invisible
		// to the metric; withdraw this only for a counter that carries a reason
		// label, never by widening the unlabelled one.
		//
		// It also sits after the error return above, so a reclaim whose unlink
		// failed is not counted although its intervals are already demoted and its
		// segment already out of the index. That under-reports rather than
		// over-reports, and the appender gets the error, so the operator learns of
		// it from the failed write rather than from a counter.
		if pressure {
			s.recordEviction(freed)
		}
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
			// decision: a shard whose fsync has permanently failed yields no
			// candidate at all, not merely no marker-bearing one. Retiring a
			// segment there means carrying its markers to a segment whose bytes
			// no fsync can be trusted to have written, and the over-refusal
			// costs nothing a broken shard was still going to deliver — its
			// durable watermark is frozen and every Commit on it already fails.
			// Skipping the shard rather than failing the pass keeps one such
			// shard from failing every writer's capacity gate.
			if sh.syncFailed.Load() {
				continue
			}
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
// eviction serves. Retrying is safe, but not because the failure changed nothing:
// the cold-flip above has already run, and carryMarkersForward is a fallible step
// after it that can fail having appended and group-committed some of the victim's
// markers. A retry therefore runs against an already-cold-flipped index and
// re-appends duplicates of those markers. Neither costs data: a duplicate carries
// the marker's ORIGINAL Version, so the recovery fold (highest Version per file)
// replays it identically, and a cold marker left for bytes that are still local
// costs a needless remote fetch, which the persist-first order already tolerates.
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

	// The markers move before the bytes go. retireSegment unlinks the file, and
	// a tombstone inside it is a delete's only durable trace.
	if err = s.carryMarkersForward(sh, seg); err != nil {
		return 0, err
	}
	return s.retireSegment(sh, seg)
}

// carryMarkersForward re-appends seg's tombstones and truncate markers to the
// shard's active segment and makes them durable, so the caller can unlink seg
// without losing the deletes it carries.
//
// Each marker keeps its ORIGINAL Version rather than minting a new one: the
// Version is what orders a marker against the records it buries, and a fresh
// one would place the delete after data written since, burying more than the
// delete did. This is the same rule repackSegment follows carrying markers to
// a repack target, and the reason neither path re-fences: the fence was taken
// when the delete was first appended.
//
// Markers stay uncounted in seg.records here too, matching the append path and
// the recovery replay, so a restart reconstructs the same counters. seg.markers
// is the separate counter, and the reason the common case is free: finding the
// markers means a scan that CRC-verifies and retains every record in the
// segment, up to a whole SegmentSize on the heap, taken while the caller holds
// flushMu and so blocking that shard's entire carve pass. Eviction touched none
// of the victim's bytes before this function existed — the bytes are already
// remote — and a segment carrying no marker must keep it that way.
//
// The append lands in the active segment rather than a fresh one on purpose. A
// marker-only segment can never be evicted (evictable requires a record) and
// never repacked (its deadBytes stay 0), so it would live for the process's
// lifetime holding an open fd — trading a data-loss bug for an fd leak bounded
// by RLIMIT_NOFILE.
//
// decision: markers are immortal AND migrating, so the set compounds. A
// segment's marker set includes whatever earlier retires carried into it, and
// every retire re-appends the whole set, so under a delete-heavy workload both
// the bytes and the per-retire work grow with the number of deletes the store
// has ever served. It is accepted because nothing on this path can tell that a
// marker has no records left to bury. Withdraw it for the rule evictable's own
// decision names — a store-wide minimum live Version, below which a marker can
// be dropped outright instead of carried.
func (s *Store) carryMarkersForward(sh *shard, seg *segmentMeta) error {
	if seg.markers.Load() == 0 {
		return nil
	}
	// decision: a shard whose fsync has permanently failed may not retire a
	// segment, however the commit below reports. groupCommit makes an fsync
	// error sticky only for waiters already enqueued; a caller enqueuing after
	// the failure gets a fresh batch and reads that fsync's own return, which
	// under Linux fsync-error semantics can be success for pages the kernel has
	// already dropped (see shard.syncFailed). This is the first place a commit's
	// success would license DESTROYING the previous durable copy, so it refuses
	// instead, the same way Restore refuses to report success. Withdraw it only
	// if an fsync failure stops being sticky.
	if sh.syncFailed.Load() {
		return fmt.Errorf("journal: cannot carry segment %d markers forward: an earlier fsync on this shard failed permanently", seg.id)
	}
	markers, err := victimMarkers(seg, s.cfg.SegmentSize)
	if err != nil {
		// Mirror repack's quarantine rather than propagating: the scan stopped
		// short, so every marker behind the torn record would be dropped, and the
		// segment must keep its bytes. corrupt takes it out of evictable and
		// pickVictim, so the pass that caught it moves on instead of re-claiming
		// the same coldest segment forever.
		seg.corrupt.Store(true)
		s.log.Warn("journal: segment failed the marker scan its retire needs; leaving it in place and skipping it",
			"segment", seg.id, "err", err)
		return err
	}
	if len(markers) == 0 {
		return nil
	}

	// Every marker framed below occupies bytes in the active segment, and the
	// retire that follows subtracts the whole of seg.tail — the markers it
	// carried included. Without this each marker is subtracted again on every
	// retire it survives, and diskBytes is only recomputed from disk at open, so
	// the drift accumulates across an uptime until MaxLocalBytes stops firing.
	// Deferred because a framed record is on disk whichever way the loop exits.
	// repackSegment accounts for the markers it carries the same way.
	var added int64
	defer func() { s.diskBytes.Add(added) }()

	sh.mu.Lock()
	for _, mk := range markers {
		if sh.active.tail.Load()+recordLen(len(mk.id), 0) > s.cfg.SegmentSize {
			// decision: the segment this seals can be marker-only, and a
			// marker-only segment is permanent — evictable requires a record and
			// pickVictim requires dead bytes, so nothing retires it. It needs an
			// active already holding nothing but carried markers, because a
			// segment's marker set fit beside at least one data record and so
			// fits in a fresh segment; the cost is one fd and that tail for the
			// process's lifetime, ceiling RLIMIT_NOFILE. The obvious fix, letting
			// evictable accept a marker-bearing segment, is wrong for a different
			// reason: markers migrate, so it would make every retire re-append
			// the whole accumulated set and charge a delete-heavy shard O(markers)
			// per delete forever.
			if err := s.sealSegment(sh); err != nil {
				sh.mu.Unlock()
				return err
			}
		}
		target := sh.active
		var werr error
		if mk.flags&flagTruncate != 0 {
			_, werr = writeTruncateRecord(target, mk.id, mk.version, mk.newSize)
		} else {
			_, werr = writeTombstoneRecord(target, mk.id, mk.version)
		}
		if werr != nil {
			sh.mu.Unlock()
			return werr
		}
		target.noteMinVersion(mk.version)
		added += recordLen(len(mk.id), 0)
	}
	sh.mu.Unlock()

	// groupCommit takes sh.mu itself, so it runs outside the loop's critical
	// section. It takes commitMu and sh.mu but never flushMu, which evictSegment
	// holds across this call.
	return sh.groupCommit()
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
	// decision: the observation hangs off entering the loop, not off the gate
	// check and not off the backoff. Recording on a check would count every
	// append on an idle store, since the gate is consulted on all of them.
	// Recording on the backoff would count only the dirty-pinned poll and drop
	// the longest stalls there are: an eviction that succeeds holds the appender
	// across evictSegment's flushMu, which waits out the shard's whole carve pass,
	// uploads included, and then clears the gate without ever reaching the
	// backoff. Loop entry means the cap was genuinely met, so every iteration of
	// it is time the appender was held, and the window runs from there to the
	// return. Withdraw only if the gate stops meaning "no room".
	var stallStart time.Time
	defer func() {
		if !stallStart.IsZero() {
			s.recordBackpressure(time.Since(stallStart))
		}
	}()
	for s.diskBytes.Load()+needed > s.cfg.MaxLocalBytes {
		if stallStart.IsZero() {
			stallStart = time.Now()
		}
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
		res, err := s.evict(ctx, overage, true)
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
