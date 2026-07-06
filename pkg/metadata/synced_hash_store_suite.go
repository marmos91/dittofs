// Package metadata — synced_hash_store_suite.go.
//
// Shared conformance suite for the SyncedHashStore interface. Each
// metadata backend (memory, badger, postgres) invokes this suite to
// prove it upholds the idempotency + isolation + concurrency contract.
//
// The suite lives in the metadata package rather than under storetest
// to keep the dependency direction clean: the interface itself lives
// here and the suite is logically paired with it. Backend test files
// in pkg/metadata/store/{memory,badger,postgres} call
// RunSyncedHashStoreSuite(t, store) from a per-backend Test*_Suite
// function.
package metadata

import (
	"context"
	"sync"
	"testing"
	"time"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block"
)

// mustHash derives a deterministic ContentHash from a string. Used to
// scope subtests on a shared store instance — each subtest picks a
// distinct seed so its state cannot collide with another subtest's.
func mustHash(seed string) block.ContentHash {
	return blake3.Sum256([]byte(seed))
}

// RunSyncedHashStoreSuite exercises the SyncedHashStore contract for a
// given backend. Intended to be invoked from a backend-local _test.go:
//
//	func TestBadgerSyncedHashStore_Suite(t *testing.T) {
//	    s := openTestStore(t)
//	    metadata.RunSyncedHashStoreSuite(t, s)
//	}
//
// Each subtest uses a distinct hash seed so subtests on a shared store
// instance do not collide. Callers MUST pass a freshly-created store —
// the suite does not reset state between subtests.
func RunSyncedHashStoreSuite(t *testing.T, s SyncedHashStore) {
	t.Helper()

	t.Run("IsSyncedBeforeMark", func(t *testing.T) {
		ctx := context.Background()
		h := mustHash("suite-is-synced-before-mark")
		got, err := s.IsSynced(ctx, h)
		if err != nil {
			t.Fatalf("IsSynced unset: %v", err)
		}
		if got {
			t.Fatalf("unset hash reports synced: got true want false")
		}
	})

	t.Run("MarkThenIsSynced", func(t *testing.T) {
		ctx := context.Background()
		h := mustHash("suite-mark-then-is-synced")

		if err := s.MarkSynced(ctx, h, block.ChunkLocator{}); err != nil {
			t.Fatalf("first MarkSynced: %v", err)
		}
		got, err := s.IsSynced(ctx, h)
		if err != nil {
			t.Fatalf("IsSynced after Mark: %v", err)
		}
		if !got {
			t.Fatalf("after MarkSynced: got false want true")
		}

		// Re-applying MarkSynced is a no-op.
		if err := s.MarkSynced(ctx, h, block.ChunkLocator{}); err != nil {
			t.Fatalf("second MarkSynced (idempotent): %v", err)
		}
		got, err = s.IsSynced(ctx, h)
		if err != nil {
			t.Fatalf("IsSynced after second Mark: %v", err)
		}
		if !got {
			t.Fatalf("after second MarkSynced: got false want true")
		}
	})

	t.Run("IsolationBetweenHashes", func(t *testing.T) {
		ctx := context.Background()
		hA := mustHash("suite-iso-a")
		hB := mustHash("suite-iso-b")

		if err := s.MarkSynced(ctx, hA, block.ChunkLocator{}); err != nil {
			t.Fatalf("MarkSynced hA: %v", err)
		}

		gotA, err := s.IsSynced(ctx, hA)
		if err != nil {
			t.Fatalf("IsSynced hA: %v", err)
		}
		gotB, err := s.IsSynced(ctx, hB)
		if err != nil {
			t.Fatalf("IsSynced hB: %v", err)
		}
		if !gotA {
			t.Fatalf("isolation: hA should be synced (got false)")
		}
		if gotB {
			t.Fatalf("isolation: hB should NOT be synced (got true)")
		}
	})

	t.Run("MarkIdempotent", func(t *testing.T) {
		ctx := context.Background()
		h := mustHash("suite-mark-idempotent")

		if err := s.MarkSynced(ctx, h, block.ChunkLocator{}); err != nil {
			t.Fatalf("first MarkSynced: %v", err)
		}
		if err := s.MarkSynced(ctx, h, block.ChunkLocator{}); err != nil {
			t.Fatalf("second MarkSynced (idempotent): %v", err)
		}
		got, err := s.IsSynced(ctx, h)
		if err != nil {
			t.Fatalf("IsSynced: %v", err)
		}
		if !got {
			t.Fatalf("after idempotent re-mark: got false want true")
		}
	})

	t.Run("GetLocatorUnsynced", func(t *testing.T) {
		ctx := context.Background()
		h := mustHash("suite-get-locator-unsynced")
		loc, ok, err := s.GetLocator(ctx, h)
		if err != nil {
			t.Fatalf("GetLocator unsynced: %v", err)
		}
		if ok {
			t.Fatalf("unsynced hash reports a locator: ok=true")
		}
		if loc != (block.ChunkLocator{}) {
			t.Fatalf("unsynced hash returned non-zero locator: %+v", loc)
		}
	})

	t.Run("StandaloneLocatorResolvesStandalone", func(t *testing.T) {
		// A chunk marked synced with a standalone locator (the PR3a path, and
		// the only on-disk form pre-PR3b) must resolve back as standalone —
		// BlockID=="" — so the read path falls back to the direct CAS GET. This
		// also covers backward compatibility: a pre-locator row carries the
		// same (no-block) state.
		ctx := context.Background()
		h := mustHash("suite-standalone-locator")
		if err := s.MarkSynced(ctx, h, block.ChunkLocator{WireLength: 1234}); err != nil {
			t.Fatalf("MarkSynced standalone: %v", err)
		}
		loc, ok, err := s.GetLocator(ctx, h)
		if err != nil {
			t.Fatalf("GetLocator standalone: %v", err)
		}
		if !ok {
			t.Fatalf("standalone synced hash reports ok=false")
		}
		if !loc.IsStandalone() {
			t.Fatalf("standalone hash resolved to a block: %+v", loc)
		}
	})

	t.Run("BlockLocatorRoundTrip", func(t *testing.T) {
		// A block locator must round-trip exactly through MarkSynced/GetLocator.
		ctx := context.Background()
		h := mustHash("suite-block-locator")
		want := block.ChunkLocator{BlockID: "block-abc123", WireOffset: 4096, WireLength: 65536}
		if err := s.MarkSynced(ctx, h, want); err != nil {
			t.Fatalf("MarkSynced block: %v", err)
		}
		got, ok, err := s.GetLocator(ctx, h)
		if err != nil {
			t.Fatalf("GetLocator block: %v", err)
		}
		if !ok {
			t.Fatalf("block-resident synced hash reports ok=false")
		}
		if got != want {
			t.Fatalf("block locator round-trip: got %+v want %+v", got, want)
		}
	})

	t.Run("DeleteSyncedAfterMark", func(t *testing.T) {
		ctx := context.Background()
		h := mustHash("suite-delete-after-mark")

		if err := s.MarkSynced(ctx, h, block.ChunkLocator{}); err != nil {
			t.Fatalf("MarkSynced: %v", err)
		}
		if err := s.DeleteSynced(ctx, h); err != nil {
			t.Fatalf("DeleteSynced: %v", err)
		}
		got, err := s.IsSynced(ctx, h)
		if err != nil {
			t.Fatalf("IsSynced after Delete: %v", err)
		}
		if got {
			t.Fatalf("after DeleteSynced: got true want false")
		}

		// Deleting an absent hash returns nil.
		if err := s.DeleteSynced(ctx, h); err != nil {
			t.Fatalf("second DeleteSynced (idempotent): %v", err)
		}
	})

	t.Run("ConcurrentMarkAndDelete", func(t *testing.T) {
		// Under concurrent goroutines alternating Mark and Delete on the
		// same hash, the backend's native concurrency primitive must
		// never panic and IsSynced must return without error. The final
		// state is non-deterministic (true or false depending on which
		// op committed last) — only the no-panic / no-error invariant is
		// asserted.
		ctx := context.Background()
		h := mustHash("suite-concurrent-mark-delete")

		const goroutines = 16
		var wg sync.WaitGroup
		for i := 0; i < goroutines; i++ {
			wg.Add(1)
			i := i
			go func() {
				defer wg.Done()
				if i%2 == 0 {
					_ = s.MarkSynced(ctx, h, block.ChunkLocator{})
				} else {
					_ = s.DeleteSynced(ctx, h)
				}
			}()
		}
		wg.Wait()

		if _, err := s.IsSynced(ctx, h); err != nil {
			t.Fatalf("IsSynced after concurrent Mark/Delete: %v", err)
		}
	})
}

// syncedHashEnumerating is the backend capability exercised by
// RunSyncedHashEnumeratorSuite: the SyncedHashStore CRUD plus the concrete
// EnumerateSynced the LIST-free GC sweep depends on. It is intentionally
// UNEXPORTED — EnumerateSynced is not part of the SyncedHashStore contract
// (only the GC consumer needs it; see the note in synced_hash_store.go), so
// this type exists solely to share the enumerator conformance across backends
// without widening the package's exported interface surface. Backend tests
// pass their concrete store; Go checks satisfaction structurally at the call.
type syncedHashEnumerating interface {
	SyncedHashStore
	EnumerateSynced(ctx context.Context, fn func(hash block.ContentHash, loc block.ChunkLocator, syncedAt time.Time) error) error
}

// RunSyncedHashEnumeratorSuite exercises a backend's EnumerateSynced against
// the LIST-free GC sweep's expectations: marked hashes appear (with a
// non-regressing first-mirror timestamp), deleted hashes do not, and a
// cancelled ctx is honored. Call alongside RunSyncedHashStoreSuite from a
// backend-local _test.go, passing a freshly-created store.
func RunSyncedHashEnumeratorSuite(t *testing.T, e syncedHashEnumerating) {
	t.Helper()

	t.Run("EnumerateSynced", func(t *testing.T) {
		ctx := context.Background()
		before := time.Now()
		hA := mustHash("enum-suite-a") // block-resident locator
		hB := mustHash("enum-suite-b") // standalone (pre-flip) locator
		hGone := mustHash("enum-suite-gone")

		// hA carries a real block locator; hB is standalone. EnumerateSynced must
		// yield each marker's locator (folded into the scan) identical to what
		// GetLocator returns — the contract that lets the migration/reconcile/
		// reclaim/compaction passes resolve locators in one scan instead of a
		// GetLocator round trip per hash (#1554).
		locA := block.ChunkLocator{BlockID: "enum-suite-block", WireOffset: 128, WireLength: 64}
		marks := map[block.ContentHash]block.ChunkLocator{hA: locA, hB: {}, hGone: {}}
		for h, loc := range marks {
			if err := e.MarkSynced(ctx, h, loc); err != nil {
				t.Fatalf("MarkSynced %x: %v", h[:4], err)
			}
		}
		// Deleting a marker must remove it from enumeration.
		if err := e.DeleteSynced(ctx, hGone); err != nil {
			t.Fatalf("DeleteSynced: %v", err)
		}

		type seenEntry struct {
			loc      block.ChunkLocator
			syncedAt time.Time
		}
		seen := make(map[block.ContentHash]seenEntry)
		if err := e.EnumerateSynced(ctx, func(h block.ContentHash, loc block.ChunkLocator, syncedAt time.Time) error {
			seen[h] = seenEntry{loc: loc, syncedAt: syncedAt}
			return nil
		}); err != nil {
			t.Fatalf("EnumerateSynced: %v", err)
		}

		if _, ok := seen[hA]; !ok {
			t.Errorf("EnumerateSynced missing marked hash hA")
		}
		if _, ok := seen[hB]; !ok {
			t.Errorf("EnumerateSynced missing marked hash hB")
		}
		if _, ok := seen[hGone]; ok {
			t.Errorf("EnumerateSynced yielded deleted hash hGone")
		}
		// The yielded locator must match GetLocator for every enumerated hash:
		// hA resolves to its block locator, hB to the zero (standalone) locator.
		if got := seen[hA].loc; got != locA {
			t.Errorf("EnumerateSynced yielded locator for hA = %+v; want %+v", got, locA)
		}
		if got := seen[hB].loc; got != (block.ChunkLocator{}) {
			t.Errorf("EnumerateSynced yielded locator for standalone hB = %+v; want zero", got)
		}
		if want, ok, err := e.GetLocator(ctx, hA); err != nil || !ok || seen[hA].loc != want {
			t.Errorf("EnumerateSynced locator for hA = %+v; GetLocator = %+v (ok=%v err=%v)", seen[hA].loc, want, ok, err)
		}
		// Standalone hB: the enumerated locator must match GetLocator too. A
		// standalone row has no block locator, so GetLocator may report ok=false;
		// either way its locator must equal the zero locator enumeration yielded.
		if want, _, err := e.GetLocator(ctx, hB); err != nil || seen[hB].loc != want {
			t.Errorf("EnumerateSynced locator for standalone hB = %+v; GetLocator = %+v (err=%v)", seen[hB].loc, want, err)
		}
		// A backend that records timestamps must report one no earlier than
		// the mark. A zero timestamp is the documented fail-closed signal for
		// backends without recorded times — accept it too.
		if ts := seen[hA].syncedAt; !ts.IsZero() && ts.Before(before.Add(-time.Minute)) {
			t.Errorf("EnumerateSynced syncedAt %v precedes mark time %v", ts, before)
		}
	})

	t.Run("EnumerateSyncedCtxCancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := e.EnumerateSynced(ctx, func(block.ContentHash, block.ChunkLocator, time.Time) error { return nil })
		if err == nil {
			t.Fatalf("EnumerateSynced with cancelled ctx: want error, got nil")
		}
	})
}
