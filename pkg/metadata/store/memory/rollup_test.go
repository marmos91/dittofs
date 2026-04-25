package memory

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

func newRollupTestStore() *MemoryMetadataStore {
	return NewMemoryMetadataStoreWithDefaults()
}

// TestMemoryRollupStore_Suite runs the shared conformance suite against the
// memory backend, so every RollupStore implementation (memory, badger,
// postgres) exercises the same contract from a single source of truth.
func TestMemoryRollupStore_Suite(t *testing.T) {
	s := newRollupTestStore()
	metadata.RunRollupStoreSuite(t, s)
}

// TestMemoryRollupStore_GetBeforeSet: querying an unset payloadID returns
// (0, nil) — a fresh file is treated as rolled-up-to-0.
func TestMemoryRollupStore_GetBeforeSet(t *testing.T) {
	s := newRollupTestStore()
	got, err := s.GetRollupOffset(context.Background(), "file1")
	if err != nil {
		t.Fatalf("GetRollupOffset unset: %v", err)
	}
	if got != 0 {
		t.Fatalf("unset rollup_offset: got %d want 0", got)
	}
}

// TestMemoryRollupStore_SetGet: Set returns previous value; Get returns
// current value; subsequent Set sees the prior value as prev.
func TestMemoryRollupStore_SetGet(t *testing.T) {
	s := newRollupTestStore()
	ctx := context.Background()

	prev, err := s.SetRollupOffset(ctx, "file1", 42)
	if err != nil {
		t.Fatalf("first Set: %v", err)
	}
	if prev != 0 {
		t.Fatalf("first Set prev: got %d want 0", prev)
	}
	got, _ := s.GetRollupOffset(ctx, "file1")
	if got != 42 {
		t.Fatalf("after first Set: got %d want 42", got)
	}

	prev, err = s.SetRollupOffset(ctx, "file1", 84)
	if err != nil {
		t.Fatalf("second Set: %v", err)
	}
	if prev != 42 {
		t.Fatalf("second Set prev: got %d want 42", prev)
	}
	got, _ = s.GetRollupOffset(ctx, "file1")
	if got != 84 {
		t.Fatalf("after second Set: got %d want 84", got)
	}
}

// TestMemoryRollupStore_RejectsRegression_KeepsPriorValue: INV-03 at the
// store layer. An attempted regression returns ErrRollupOffsetRegression and
// leaves the stored value untouched.
func TestMemoryRollupStore_RejectsRegression_KeepsPriorValue(t *testing.T) {
	s := newRollupTestStore()
	ctx := context.Background()

	if _, err := s.SetRollupOffset(ctx, "file1", 100); err != nil {
		t.Fatalf("initial Set: %v", err)
	}

	prev, err := s.SetRollupOffset(ctx, "file1", 50)
	if !errors.Is(err, metadata.ErrRollupOffsetRegression) {
		t.Fatalf("want ErrRollupOffsetRegression, got %v", err)
	}
	if prev != 100 {
		t.Fatalf("regression prev: got %d want 100", prev)
	}

	got, _ := s.GetRollupOffset(ctx, "file1")
	if got != 100 {
		t.Fatalf("stored value regressed despite error: got %d want 100", got)
	}
}

// TestMemoryRollupStore_EqualOffsetNotRegression: Set(N) after Set(N) is
// not a regression — the monotone test is strict-less-than.
func TestMemoryRollupStore_EqualOffsetNotRegression(t *testing.T) {
	s := newRollupTestStore()
	ctx := context.Background()

	if _, err := s.SetRollupOffset(ctx, "file1", 200); err != nil {
		t.Fatalf("initial Set: %v", err)
	}
	prev, err := s.SetRollupOffset(ctx, "file1", 200)
	if err != nil {
		t.Fatalf("equal Set: got %v want nil", err)
	}
	if prev != 200 {
		t.Fatalf("equal Set prev: got %d want 200", prev)
	}
	got, _ := s.GetRollupOffset(ctx, "file1")
	if got != 200 {
		t.Fatalf("after equal Set: got %d want 200", got)
	}
}

// TestMemoryRollupStore_IndependentPayloads: different payloadIDs have
// independent rollup_offset storage.
func TestMemoryRollupStore_IndependentPayloads(t *testing.T) {
	s := newRollupTestStore()
	ctx := context.Background()
	if _, err := s.SetRollupOffset(ctx, "file-a", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRollupOffset(ctx, "file-b", 20); err != nil {
		t.Fatal(err)
	}
	a, _ := s.GetRollupOffset(ctx, "file-a")
	b, _ := s.GetRollupOffset(ctx, "file-b")
	if a != 10 || b != 20 {
		t.Fatalf("independent payload offsets: got a=%d b=%d want 10,20", a, b)
	}
}

// TestMemoryRollupStore_ConcurrentMonotone: concurrent goroutines attempting
// to advance + regress never leave the store in a regressed state.
func TestMemoryRollupStore_ConcurrentMonotone(t *testing.T) {
	s := newRollupTestStore()
	ctx := context.Background()
	if _, err := s.SetRollupOffset(ctx, "hot", 1000); err != nil {
		t.Fatal(err)
	}

	const goroutines = 16
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			// Half try to advance, half try to regress.
			if i%2 == 0 {
				_, _ = s.SetRollupOffset(ctx, "hot", uint64(1000+i))
			} else {
				_, _ = s.SetRollupOffset(ctx, "hot", uint64(i))
			}
		}()
	}
	wg.Wait()

	got, _ := s.GetRollupOffset(ctx, "hot")
	if got < 1000 {
		t.Fatalf("concurrent regression: got %d, must be >= 1000 (INV-03)", got)
	}
}
