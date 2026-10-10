package memory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journal"
)

func memoryBytes(t *testing.T, s *MemoryStore, id journal.FileID, n int) []byte {
	t.Helper()
	got := make([]byte, n)
	if _, _, err := s.ReadAt(context.Background(), id, 0, got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestHydrateFillsGapsWithoutReplacingResidentBytes(t *testing.T) {
	for _, current := range []bool{false, true} {
		t.Run(fmt.Sprintf("current_bound=%v", current), func(t *testing.T) {
			ctx, s := context.Background(), New()
			at := s.WriteVersion()
			if err := s.WriteAt(ctx, "file", 2, []byte("NEW")); err != nil {
				t.Fatal(err)
			}
			if current {
				at = s.WriteVersion()
			}
			if err := s.Hydrate(ctx, "file", 0, []byte("abcdefgh"), at); err != nil {
				t.Fatal(err)
			}
			if got := memoryBytes(t, s, "file", 8); !bytes.Equal(got, []byte("abNEWfgh")) {
				t.Fatalf("hydrate replaced resident bytes: %q", got)
			}
			if s.UnsyncedBytes() != 3 {
				t.Fatalf("hydrate changed dirty accounting: %d", s.UnsyncedBytes())
			}
			if err := s.Hydrate(ctx, "file", 0, bytes.Repeat([]byte{'x'}, 8), s.WriteVersion()); err != nil {
				t.Fatal(err)
			}
			if got := memoryBytes(t, s, "file", 8); !bytes.Equal(got, []byte("abNEWfgh")) {
				t.Fatalf("current-bound refill replaced resident bytes: %q", got)
			}
		})
	}
}

func TestHydrateRejectsRemovedRangesAndAllowsFreshFill(t *testing.T) {
	for _, mutation := range []string{"truncate", "truncate-absent", "delete", "delete-recreate-truncate", "evicted-fence"} {
		t.Run(mutation, func(t *testing.T) {
			ctx, s := context.Background(), New()
			if mutation != "truncate-absent" {
				if err := s.WriteAt(ctx, "file", 0, []byte("original")); err != nil {
					t.Fatal(err)
				}
			}
			at := s.WriteVersion()
			var err error
			switch mutation {
			case "truncate", "truncate-absent":
				err = s.Truncate(ctx, "file", 0)
			default:
				err = s.Delete(ctx, "file")
			}
			if err != nil {
				t.Fatal(err)
			}
			if mutation == "delete-recreate-truncate" {
				if err := s.WriteAt(ctx, "file", 8, []byte("tail")); err != nil {
					t.Fatal(err)
				}
				if err := s.Truncate(ctx, "file", 10); err != nil {
					t.Fatal(err)
				}
			}
			if mutation == "evicted-fence" {
				for i := 0; i < maxHydrateFences; i++ {
					if err := s.Delete(ctx, journal.FileID(fmt.Sprintf("other-%d", i))); err != nil {
						t.Fatal(err)
					}
				}
				if len(s.fences) > maxHydrateFences || len(s.fenceOrder) > maxHydrateFences {
					t.Fatal("mutation fences grew past their bound")
				}
			}
			if s.WriteVersion() <= at {
				t.Fatal("mutation did not advance the sampled write version")
			}
			if err := s.Hydrate(ctx, "file", 0, []byte("stale"), at); mutation == "evicted-fence" {
				if !errors.Is(err, ErrHydrateHistoryExpired) {
					t.Fatalf("expired plan must fail explicitly: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if got := memoryBytes(t, s, "file", 5); !bytes.Equal(got, make([]byte, 5)) {
				t.Fatalf("stale hydrate restored removed bytes: %q", got)
			}
			if err := s.Hydrate(ctx, "file", 0, []byte("fresh"), s.WriteVersion()); err != nil {
				t.Fatal(err)
			}
			if got := memoryBytes(t, s, "file", 5); !bytes.Equal(got, []byte("fresh")) {
				t.Fatalf("fresh hydrate was not admitted: %q", got)
			}
		})
	}
}

func TestHydrateFillsPrefixSurvivingOnlyPostPlanFences(t *testing.T) {
	ctx, s := context.Background(), New()
	if err := s.Delete(ctx, "file"); err != nil {
		t.Fatal(err)
	}
	at := s.WriteVersion()
	if err := s.Truncate(ctx, "file", 4); err != nil {
		t.Fatal(err)
	}
	if err := s.Hydrate(ctx, "file", 0, []byte("abcdefgh"), at); err != nil {
		t.Fatal(err)
	}
	if got := memoryBytes(t, s, "file", 8); !bytes.Equal(got, []byte{'a', 'b', 'c', 'd', 0, 0, 0, 0}) {
		t.Fatalf("valid prefix was refused by a pre-plan delete or straddling truncate: %q", got)
	}
}

func TestHydrateResidentNoOpNeedsNoRetainedHistory(t *testing.T) {
	ctx, s := context.Background(), New()
	for i := 0; i <= maxHydrateFences; i++ {
		if err := s.Delete(ctx, "other"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.WriteAt(ctx, "file", 0, []byte("current")); err != nil {
		t.Fatal(err)
	}
	if err := s.Hydrate(ctx, "file", 0, []byte("obsolete"), 0); !errors.Is(err, ErrHydrateHistoryExpired) {
		t.Fatalf("uncovered expired range did not fail: %v", err)
	}
	if err := s.Hydrate(ctx, "file", 0, []byte("old"), 0); err != nil {
		t.Fatalf("resident no-op refused an irrelevant bound: %v", err)
	}
	if got := memoryBytes(t, s, "file", 7); !bytes.Equal(got, []byte("current")) {
		t.Fatalf("resident bytes changed: %q", got)
	}
}

func TestTruncateAccountsOnlySurvivingDirtyRanges(t *testing.T) {
	ctx, s := context.Background(), New()
	if err := s.Hydrate(ctx, "file", 0, make([]byte, 16), 0); err != nil {
		t.Fatal(err)
	}
	for _, r := range [][2]int64{{2, 6}, {4, 8}, {12, 16}} {
		if err := s.WriteAt(ctx, "file", r[0], bytes.Repeat([]byte{'x'}, int(r[1]-r[0]))); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.UnsyncedBytes(); got != 10 {
		t.Fatalf("overlapping writes double-counted dirty bytes: %d", got)
	}
	for _, tc := range []struct{ size, dirty int64 }{{14, 8}, {10, 6}, {5, 3}, {1, 0}, {0, 0}} {
		if err := s.Truncate(ctx, "file", tc.size); err != nil {
			t.Fatal(err)
		}
		if got := s.UnsyncedBytes(); got != tc.dirty {
			t.Fatalf("truncate(%d) dirty bytes=%d, want %d", tc.size, got, tc.dirty)
		}
		dirty, err := s.HasDirty(ctx, "file")
		if err != nil || dirty != (tc.dirty > 0) {
			t.Fatalf("truncate(%d) HasDirty=%v error=%v", tc.size, dirty, err)
		}
	}
}
