// short-circuit unit tests.
//
// (TDD-RED): tests written against the contract.
// (GREEN): implementation flips them to PASS.
//
// for the exact semantics
//
// -: speculative pre-upload short-circuit (chunk locally, look up
//     by Merkle-root ObjectID BEFORE any S3 PUT).
// -: trigger condition — len(Blocks)>0 AND every BlockState==Pending
//     AND file.ObjectID == zero.
// -: on hit, RefCount++ on every distinct hash in target.Blocks
//     persist FileAttr.Blocks = target.Blocks (deep copy), persist
// FileAttr.ObjectID = provisional, decrement speculative-only hashes
//     invalidate cache for orphaned speculative chunks, truncate local
//     append-log immediately.
// -: local append-log + chunker rollup files truncated immediately.
// -: concurrent-quiesce race — first-committer wins via the partial
//     UNIQUE index; loser detects the conflict, decrements its just-uploaded
// blocks, re-calls FindByObjectID, swaps to target's BlockRef list
//     and re-commits.
//
// The stub `trySpeculativeFileLevelDedup` always returns
// (false, nil); these tests therefore FAIL on the current codebase and
// PASS unmodified after wires the implementation. That is the
// desired RED state — the assertions encode the contract.

package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// dedupTestSetup wires a *Syncer with a *fakeCoordinator and a fresh
// in-memory local store, mirroring the api_blockref_test.go pattern.
// Returns the syncer + coord so each scenario can seed objectIDHits and
// invoke trySpeculativeFileLevelDedup directly.
//
// may rebind the entry point onto a different receiver; the
// helper is intentionally minimal so it stays adaptable.
func dedupTestSetup(t *testing.T) (*Syncer, *fakeCoordinator) {
	t.Helper()
	fc := newFakeCoordinator()
	bs := newTestEngineWithCoordinator(t, fc)
	if bs.syncer == nil {
		t.Fatalf("BlockStore.syncer is nil after construction with fakeCoordinator")
	}
	return bs.syncer, fc
}

// makeSpeculativeBlocks builds N BlockRefs sorted by Offset with hashes
// derived from the supplied byte tags. Each tag becomes the FIRST byte
// of the ContentHash so tests can craft predictable distinct hashes.
func makeSpeculativeBlocks(tags ...byte) []blockstore.BlockRef {
	out := make([]blockstore.BlockRef, len(tags))
	for i, tag := range tags {
		var h blockstore.ContentHash
		h[0] = tag
		out[i] = blockstore.BlockRef{
			Hash:   h,
			Offset: uint64(i) * 4096,
			Size:   4096,
		}
	}
	return out
}

// pendingStates returns a parallel BlockState slice with every entry
// set to Pending — the trigger requires "every block.State ==
// Pending".
func pendingStates(n int) []blockstore.BlockState {
	out := make([]blockstore.BlockState, n)
	for i := range out {
		out[i] = blockstore.BlockStatePending
	}
	return out
}

// TestDedup_TriggerCondition encodes: short-circuit fires only
// when ALL THREE of these hold
//
//   - len(Blocks) > 0
//   - every BlockState == Pending
//   - file.ObjectID == zero
//
// Each negation is asserted as a sub-test: each one MUST NOT trigger a
// FindByObjectID call. The positive (all three satisfied) sub-test
// MUST trigger exactly one FindByObjectID call.
//
// On the stub, the negative sub-tests pass trivially (the stub
// never calls FindByObjectID). The positive sub-test FAILS —
// will flip it to PASS by issuing the lookup.
func TestDedup_TriggerCondition(t *testing.T) {
	ctx := context.Background()

	t.Run("EmptyBlocks_NoFindCall", func(t *testing.T) {
		m, fc := dedupTestSetup(t)
		hit, err := m.trySpeculativeFileLevelDedup(ctx, "pid", nil, blockstore.ObjectID{}, nil)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if hit {
			t.Errorf("hit=true on empty blocks; want false (D-09)")
		}
		if got := len(fc.findCalls); got != 0 {
			t.Errorf("findCalls=%d on empty blocks; want 0 (D-09 trigger requires len>0)", got)
		}
	})

	t.Run("OneBlockNotPending_NoFindCall", func(t *testing.T) {
		m, fc := dedupTestSetup(t)
		blocks := makeSpeculativeBlocks(0xA1, 0xA2, 0xA3)
		states := pendingStates(3)
		states[1] = blockstore.BlockStateRemote // one already Remote
		hit, err := m.trySpeculativeFileLevelDedup(ctx, "pid", blocks, blockstore.ObjectID{}, states)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if hit {
			t.Errorf("hit=true with one Remote block; want false (D-09)")
		}
		if got := len(fc.findCalls); got != 0 {
			t.Errorf("findCalls=%d with one Remote block; want 0 (D-09)", got)
		}
	})

	t.Run("FileObjectIDNonZero_NoFindCall", func(t *testing.T) {
		m, fc := dedupTestSetup(t)
		blocks := makeSpeculativeBlocks(0xA1, 0xA2)
		states := pendingStates(2)
		// Already-quiesced file: ObjectID non-zero.
		var prior blockstore.ObjectID
		prior[0] = 0xFF
		hit, err := m.trySpeculativeFileLevelDedup(ctx, "pid", blocks, prior, states)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if hit {
			t.Errorf("hit=true with non-zero file ObjectID; want false (D-09)")
		}
		if got := len(fc.findCalls); got != 0 {
			t.Errorf("findCalls=%d with non-zero file ObjectID; want 0 (D-09)", got)
		}
	})

	t.Run("AllConditionsMet_FindCalledOnce", func(t *testing.T) {
		m, fc := dedupTestSetup(t)
		blocks := makeSpeculativeBlocks(0xA1, 0xA2, 0xA3)
		states := pendingStates(3)
		// objectIDHits not seeded — this sub-test asserts the LOOKUP
		// fires; HitFlow exercises the response side.
		_, err := m.trySpeculativeFileLevelDedup(ctx, "pid", blocks, blockstore.ObjectID{}, states)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if got := len(fc.findCalls); got != 1 {
			t.Errorf("findCalls=%d when all three trigger conditions hold; want 1 (D-09)", got)
		}
		if len(fc.findCalls) == 1 {
			want := blockstore.ComputeObjectID(blocks)
			if fc.findCalls[0] != want {
				t.Errorf("FindByObjectID arg=%s; want provisional ObjectID %s",
					fc.findCalls[0].String(), want.String())
			}
		}
	})
}

// TestDedup_ShortCircuit_HitFlow encodes: on a seeded objectIDHits
// match, the short-circuit
//
//   - calls IncrementRefCount once per distinct target hash
//   - does NOT issue any remote-store WriteBlockWithHash PUT
//   - calls PersistFileBlocks with target's BlockRefs and the provisional
//     ObjectID in a single metadata txn
//
// stub returns (false, nil) and performs no work — this test
// fails on every assertion and flips it.
func TestDedup_ShortCircuit_HitFlow(t *testing.T) {
	ctx := context.Background()
	m, fc := dedupTestSetup(t)

	speculative := makeSpeculativeBlocks(0xA1, 0xA2, 0xA3)
	provisional := blockstore.ComputeObjectID(speculative)

	// Target file already exists with three distinct hashes (T1/T2/T3).
	target := makeSpeculativeBlocks(0xB1, 0xB2, 0xB3)
	fc.objectIDHits[provisional] = target

	hit, err := m.trySpeculativeFileLevelDedup(ctx, "pid",
		speculative, blockstore.ObjectID{}, pendingStates(len(speculative)))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !hit {
		t.Fatalf("hit=false on seeded objectIDHits; want true (D-10)")
	}

	// IncrementRefCount called once per distinct target hash.
	if got := len(fc.incHashes); got != 3 {
		t.Errorf("IncrementRefCount calls=%d; want 3 (one per target hash) (D-10)", got)
	}
	seen := make(map[blockstore.ContentHash]bool, 3)
	for _, h := range fc.incHashes {
		seen[h] = true
	}
	for _, want := range target {
		if !seen[want.Hash] {
			t.Errorf("expected IncrementRefCount on %s; not seen (D-10)", want.Hash.String())
		}
	}

	// PersistFileBlocks called once with target's blocks + provisional ObjectID.
	if got := len(fc.persistCalls); got != 1 {
		t.Fatalf("PersistFileBlocks calls=%d; want 1 (D-10 single-txn write)", got)
	}
	rec := fc.persistCalls[0]
	if rec.payloadID != "pid" {
		t.Errorf("PersistFileBlocks payloadID=%q; want %q", rec.payloadID, "pid")
	}
	if len(rec.blocks) != len(target) {
		t.Errorf("PersistFileBlocks blocks len=%d; want %d (target BlockRef list)",
			len(rec.blocks), len(target))
	}
	if rec.objectID != provisional {
		t.Errorf("PersistFileBlocks objectID=%s; want provisional %s (D-10)",
			rec.objectID.String(), provisional.String())
	}
}

// TestDedup_ShortCircuit_MissFlow encodes the miss path of: when
// FindByObjectID returns no hit, trySpeculativeFileLevelDedup returns
// (false, nil) — the caller proceeds with the existing per-block path.
//
//   - FindByObjectID called exactly once.
//   - PersistFileBlocks NOT called from within the short-circuit
//     (the per-block path's post-Flush hook persists later).
//   - IncrementRefCount NOT called (no target to RefCount-bump).
//
// On the stub the short-circuit doesn't run at all, so the
// FindByObjectID assertion fails. fires the lookup, gets a
// miss, and returns (false, nil) — every assertion holds.
func TestDedup_ShortCircuit_MissFlow(t *testing.T) {
	ctx := context.Background()
	m, fc := dedupTestSetup(t)

	speculative := makeSpeculativeBlocks(0xA1, 0xA2, 0xA3)

	hit, err := m.trySpeculativeFileLevelDedup(ctx, "pid",
		speculative, blockstore.ObjectID{}, pendingStates(len(speculative)))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if hit {
		t.Errorf("hit=true on miss; want false (D-08)")
	}
	if got := len(fc.findCalls); got != 1 {
		t.Errorf("findCalls=%d; want 1 (miss path still issues the lookup) (D-08)", got)
	}
	if got := len(fc.incHashes); got != 0 {
		t.Errorf("IncrementRefCount calls=%d on miss; want 0", got)
	}
	if got := len(fc.persistCalls); got != 0 {
		t.Errorf("PersistFileBlocks calls=%d on miss; want 0 "+
			"(post-Flush hook persists later, not the short-circuit)", got)
	}
}

// TestDedup_RefCountMath encodes step 4 +
//
//   - speculative file has hashes {A, B, C, D}
//
// - target file has hashes {A, C, E}
//
// After the short-circuit hit, observable RefCount calls MUST be
//
//   - IncrementRefCount on each distinct target hash: A, C, E (3 total).
//   - DecrementRefCount on each speculative-only hash: B, D (2 total).
//
// The stub does nothing → 0/0; must produce 3/2.
//
// (The pre-claimed RefCount for speculative-only hashes B and D was
// bumped by the per-block path BEFORE the short-circuit; the swap must
// undo those so the file ends up referencing only target's blocks.)
func TestDedup_RefCountMath(t *testing.T) {
	ctx := context.Background()
	m, fc := dedupTestSetup(t)

	// Build speculative {A=0xA1, B=0xA2, C=0xA3, D=0xA4}.
	speculative := makeSpeculativeBlocks(0xA1, 0xA2, 0xA3, 0xA4)
	// Target {A=0xA1, C=0xA3, E=0xA5} — A and C are shared with speculative.
	target := makeSpeculativeBlocks(0xA1, 0xA3, 0xA5)
	provisional := blockstore.ComputeObjectID(speculative)
	fc.objectIDHits[provisional] = target

	hit, err := m.trySpeculativeFileLevelDedup(ctx, "pid",
		speculative, blockstore.ObjectID{}, pendingStates(len(speculative)))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !hit {
		t.Fatalf("hit=false; want true (D-10)")
	}

	// Increments: A, C, E (target's distinct hashes).
	if got := len(fc.incHashes); got != 3 {
		t.Errorf("IncrementRefCount calls=%d; want 3 (target hashes A, C, E)", got)
	}
	wantInc := map[byte]bool{0xA1: false, 0xA3: false, 0xA5: false}
	for _, h := range fc.incHashes {
		wantInc[h[0]] = true
	}
	for tag, seen := range wantInc {
		if !seen {
			t.Errorf("expected IncrementRefCount on hash with leading byte 0x%02X; not seen", tag)
		}
	}

	// Decrements: B, D (speculative-only).
	if got := len(fc.decHashes); got != 2 {
		t.Errorf("DecrementRefCount calls=%d; want 2 (speculative-only hashes B, D) (INV-02)", got)
	}
	wantDec := map[byte]bool{0xA2: false, 0xA4: false}
	for _, h := range fc.decHashes {
		wantDec[h[0]] = true
	}
	for tag, seen := range wantDec {
		if !seen {
			t.Errorf("expected DecrementRefCount on hash with leading byte 0x%02X; not seen", tag)
		}
	}
}

// TestDedup_CacheInvalidation encodes step 5 +: after
// the short-circuit hit, the engine's Cache MUST be invalidated for the
// speculative chunks that did NOT make it into target's BlockRef list
// (orphans — they got pre-claimed in fileBlockStore.Put before the
// dedup attempt and now have a RefCount but no FileAttr.Blocks reference).
//
// Speculative {A, B, C}; target {A, C}. Expected InvalidateFile call
// with removed = [B] (and ONLY B — A and C are still owned via target).
//
// On the stub: cache is never invalidated → invalidateCalls = 0
// and assertion fails. wires the call.
func TestDedup_CacheInvalidation(t *testing.T) {
	ctx := context.Background()
	_, fc := dedupTestSetup(t)

	rec := &recordingCache{}
	// BlockStore.cache is package-private; the helper exposed it earlier
	// (see TestClose_ClosesCache pattern in engine_test.go). The Syncer
	// cannot invalidate without the BlockStore reference; will
	// thread the call site appropriately. We attach the recorder via the
	// *BlockStore that owns the syncer.
	bs := newTestEngineWithCoordinator(t, fc)
	bs.cache = rec
	m := bs.syncer

	speculative := makeSpeculativeBlocks(0xA1, 0xA2, 0xA3) // A, B, C
	target := makeSpeculativeBlocks(0xA1, 0xA3)            // A, C
	provisional := blockstore.ComputeObjectID(speculative)
	fc.objectIDHits[provisional] = target

	hit, err := m.trySpeculativeFileLevelDedup(ctx, "pid",
		speculative, blockstore.ObjectID{}, pendingStates(len(speculative)))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !hit {
		t.Fatalf("hit=false; want true (D-10)")
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.invalidateCalls != 1 {
		t.Errorf("Cache.InvalidateFile calls=%d; want 1 (D-10 step 5)", rec.invalidateCalls)
	}
	if rec.lastPayloadID != "pid" {
		t.Errorf("InvalidateFile payloadID=%q; want %q", rec.lastPayloadID, "pid")
	}
	if len(rec.lastInvHashes) != 1 {
		t.Errorf("InvalidateFile removed-hashes len=%d; want 1 (B only) (D-10)",
			len(rec.lastInvHashes))
	}
	if len(rec.lastInvHashes) == 1 && rec.lastInvHashes[0][0] != 0xA2 {
		t.Errorf("InvalidateFile removed-hash[0]=%s; want B (leading 0xA2)",
			rec.lastInvHashes[0].String())
	}
}

// fakePgConflictError is a stand-in for the Postgres unique-violation
// (23505) the loser detects on the partial UNIQUE index over
// files.object_id. should treat this as the
// concurrent-quiesce-race signal: decrement just-uploaded refcounts
// re-look-up via FindByObjectID, swap to target's BlockRefs, retry
// PersistFileBlocks.
type fakePgConflictError struct{}

func (e *fakePgConflictError) Error() string { return "fake Postgres unique violation (23505)" }

// IsConflict satisfies a hypothetical interface the engine may use to
// detect the race-loser signal without coupling to pgx error codes.
// If a future change picks a different sentinel (errors.Is on a
// blockstore.ErrConflict), the test still compiles — it just relies on
// the surrounding flow assertions, not on this sentinel directly.
func (e *fakePgConflictError) IsConflict() bool { return true }

// TestDedup_ConcurrentRace encodes: two clones quiesce
// simultaneously; first commits (no race), second detects the
// unique-violation, decrements its just-uploaded blocks, swaps to the
// (now-existing) target's BlockRefs, and re-commits.
//
// Setup: persistErr is set to a *fakePgConflictError on the FIRST
// PersistFileBlocks call. The retry path MUST then
//
//  1. (Optionally) re-call FindByObjectID — the target now exists.
//  2. Decrement RefCount on the just-uploaded speculative-only blocks.
//  3. Re-call PersistFileBlocks — this time persistErr is cleared and
//     the call succeeds.
//
// Net observable: at least 2 PersistFileBlocks calls (the first
// errored, the second succeeded); the file's final BlockRef list
// equals target.
//
// On a no-op stub: 0 PersistFileBlocks calls → assertion fails. The
// retry loop wires this path.
func TestDedup_ConcurrentRace(t *testing.T) {
	ctx := context.Background()
	m, fc := dedupTestSetup(t)

	speculative := makeSpeculativeBlocks(0xA1, 0xA2, 0xA3)
	target := makeSpeculativeBlocks(0xB1, 0xB2)
	provisional := blockstore.ComputeObjectID(speculative)
	// Pre-seed: when the loser RE-looks up after detecting the conflict
	// the target is already there.
	fc.objectIDHits[provisional] = target
	// Inject conflict on the first PersistFileBlocks (single-shot).
	fc.persistErr = &fakePgConflictError{}

	hit, err := m.trySpeculativeFileLevelDedup(ctx, "pid",
		speculative, blockstore.ObjectID{}, pendingStates(len(speculative)))
	if err != nil {
		t.Fatalf("err: %v (loser must absorb the conflict and retry, not surface it)", err)
	}
	if !hit {
		t.Fatalf("hit=false; want true after retry (D-14)")
	}

	// At least 2 PersistFileBlocks calls: first errored, second succeeded.
	if got := len(fc.persistCalls); got < 2 {
		t.Errorf("PersistFileBlocks calls=%d; want >= 2 (first errored, retry succeeded) (D-14)", got)
	}

	// Final committed blocks equal target.
	if len(fc.persistCalls) > 0 {
		final := fc.persistCalls[len(fc.persistCalls)-1]
		if len(final.blocks) != len(target) {
			t.Errorf("final PersistFileBlocks blocks len=%d; want %d (target list)",
				len(final.blocks), len(target))
		}
		if final.objectID != provisional {
			t.Errorf("final PersistFileBlocks objectID=%s; want provisional %s",
				final.objectID.String(), provisional.String())
		}
	}

	// Sanity: persistErr drained (the single-shot fired exactly once).
	if fc.persistErr != nil {
		t.Errorf("persistErr still armed after retry; should be drained (single-shot)")
	}
}

// Compile-time check: keep errors import live even if reshapes
// the error-handling path. Tests that need errors.Is can adopt this.
var _ = errors.Is
