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

		if err := s.MarkSynced(ctx, h); err != nil {
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
		if err := s.MarkSynced(ctx, h); err != nil {
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

		if err := s.MarkSynced(ctx, hA); err != nil {
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

		if err := s.MarkSynced(ctx, h); err != nil {
			t.Fatalf("first MarkSynced: %v", err)
		}
		if err := s.MarkSynced(ctx, h); err != nil {
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

	t.Run("DeleteSyncedAfterMark", func(t *testing.T) {
		ctx := context.Background()
		h := mustHash("suite-delete-after-mark")

		if err := s.MarkSynced(ctx, h); err != nil {
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
					_ = s.MarkSynced(ctx, h)
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
