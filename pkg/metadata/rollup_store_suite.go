// Package metadata — rollup_store_suite.go (Phase 10).
//
// Shared conformance suite for the RollupStore interface. Each metadata
// backend (memory, badger, postgres) exercises this suite to prove it
// upholds the INV-03 atomic-monotone contract.
//
// The suite lives in the metadata package rather than storetest to keep
// the dependency direction clean: storetest already depends on metadata,
// so moving this helper into storetest would be fine structurally, but
// the RollupStore interface itself lives here and the suite is logically
// paired with it. See pkg/metadata/rollup_store.go for the contract.
package metadata

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// RunRollupStoreSuite exercises the RollupStore contract for a given
// backend. Intended to be invoked from a backend-local _test.go:
//
//	func TestBadgerRollupStore_Suite(t *testing.T) {
//	    s := openTestStore(t)
//	    metadata.RunRollupStoreSuite(t, s)
//	}
//
// Each subtest uses an isolated payloadID so suite subtests do not
// interfere with each other when the same store instance is shared.
// Callers MUST pass a freshly-created store — the suite does not reset
// state between subtests.
func RunRollupStoreSuite(t *testing.T, rs RollupStore) {
	t.Helper()

	t.Run("GetBeforeSet", func(t *testing.T) {
		got, err := rs.GetRollupOffset(context.Background(), "suite-get-before-set")
		if err != nil {
			t.Fatalf("GetRollupOffset unset: %v", err)
		}
		if got != 0 {
			t.Fatalf("unset rollup_offset: got %d want 0", got)
		}
	})

	t.Run("SetGet", func(t *testing.T) {
		ctx := context.Background()
		const pid = "suite-set-get"

		prev, err := rs.SetRollupOffset(ctx, pid, 42)
		if err != nil {
			t.Fatalf("first Set: %v", err)
		}
		if prev != 0 {
			t.Fatalf("first Set prev: got %d want 0", prev)
		}
		got, err := rs.GetRollupOffset(ctx, pid)
		if err != nil {
			t.Fatalf("Get after first Set: %v", err)
		}
		if got != 42 {
			t.Fatalf("after first Set: got %d want 42", got)
		}

		prev, err = rs.SetRollupOffset(ctx, pid, 84)
		if err != nil {
			t.Fatalf("second Set: %v", err)
		}
		if prev != 42 {
			t.Fatalf("second Set prev: got %d want 42", prev)
		}
		got, err = rs.GetRollupOffset(ctx, pid)
		if err != nil {
			t.Fatalf("Get after second Set: %v", err)
		}
		if got != 84 {
			t.Fatalf("after second Set: got %d want 84", got)
		}
	})

	t.Run("IsolationBetweenPayloadIDs", func(t *testing.T) {
		ctx := context.Background()
		if _, err := rs.SetRollupOffset(ctx, "suite-iso-a", 100); err != nil {
			t.Fatal(err)
		}
		if _, err := rs.SetRollupOffset(ctx, "suite-iso-b", 200); err != nil {
			t.Fatal(err)
		}
		a, err := rs.GetRollupOffset(ctx, "suite-iso-a")
		if err != nil {
			t.Fatal(err)
		}
		b, err := rs.GetRollupOffset(ctx, "suite-iso-b")
		if err != nil {
			t.Fatal(err)
		}
		if a != 100 || b != 200 {
			t.Fatalf("isolation: a=%d b=%d want 100,200", a, b)
		}
	})

	t.Run("SetRollupOffset_RejectsRegression_KeepsPriorValue", func(t *testing.T) {
		// INV-03 enforced at the STORE layer (Phase 10 Blocker 2 fix).
		ctx := context.Background()
		const pid = "suite-regression"

		if _, err := rs.SetRollupOffset(ctx, pid, 500); err != nil {
			t.Fatalf("initial Set: %v", err)
		}

		prev, err := rs.SetRollupOffset(ctx, pid, 100)
		if !errors.Is(err, ErrRollupOffsetRegression) {
			t.Fatalf("want ErrRollupOffsetRegression, got err=%v prev=%d", err, prev)
		}
		if prev != 500 {
			t.Fatalf("regression prev: got %d want 500", prev)
		}

		// Stored value MUST NOT have regressed.
		got, err := rs.GetRollupOffset(ctx, pid)
		if err != nil {
			t.Fatalf("Get after regression: %v", err)
		}
		if got != 500 {
			t.Fatalf("stored offset regressed despite ErrRollupOffsetRegression: got %d want 500", got)
		}
	})

	t.Run("SetRollupOffset_AllowsEqualValue", func(t *testing.T) {
		// newOffset == stored is NOT a regression (idempotent re-apply).
		ctx := context.Background()
		const pid = "suite-equal"

		if _, err := rs.SetRollupOffset(ctx, pid, 777); err != nil {
			t.Fatalf("initial Set: %v", err)
		}
		prev, err := rs.SetRollupOffset(ctx, pid, 777)
		if err != nil {
			t.Fatalf("equal re-apply must not regress: %v", err)
		}
		if prev != 777 {
			t.Fatalf("equal re-apply prev: got %d want 777", prev)
		}
		got, err := rs.GetRollupOffset(ctx, pid)
		if err != nil {
			t.Fatal(err)
		}
		if got != 777 {
			t.Fatalf("after equal re-apply: got %d want 777", got)
		}
	})

	t.Run("ConcurrentMonotone", func(t *testing.T) {
		// Under concurrent goroutines racing to advance vs. regress, the
		// final stored value must never be below the highest successful
		// advance. This exercises the backend's native concurrency
		// primitive (mutex, Badger txn, Postgres WHERE guard).
		ctx := context.Background()
		const pid = "suite-concurrent"

		if _, err := rs.SetRollupOffset(ctx, pid, 1000); err != nil {
			t.Fatal(err)
		}

		const goroutines = 16
		var wg sync.WaitGroup
		for i := 0; i < goroutines; i++ {
			wg.Add(1)
			i := i
			go func() {
				defer wg.Done()
				if i%2 == 0 {
					// Try to advance.
					_, _ = rs.SetRollupOffset(ctx, pid, uint64(1000+i))
				} else {
					// Try to regress — must be rejected.
					_, _ = rs.SetRollupOffset(ctx, pid, uint64(i))
				}
			}()
		}
		wg.Wait()

		got, err := rs.GetRollupOffset(ctx, pid)
		if err != nil {
			t.Fatal(err)
		}
		if got < 1000 {
			t.Fatalf("concurrent regression: got %d, must be >= 1000 (INV-03)", got)
		}
	})
}
