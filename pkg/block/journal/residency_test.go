package journal

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
)

func TestIsRangeResident(t *testing.T) {
	s := testStore(t, Config{})
	ctx := context.Background()
	if err := s.WriteAt(ctx, "f", 0, make([]byte, 8)); err != nil {
		t.Fatal(err)
	}
	if err := s.Hydrate(ctx, "f", 8, make([]byte, 8), 0); err != nil {
		t.Fatal(err)
	}
	if err := s.SeedCold(ctx, "f", [][2]int64{{16, 8}}); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteAt(ctx, "f", 24, make([]byte, 8)); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteAt(ctx, "f", 40, make([]byte, 8)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		off, n int64
		want   bool
	}{
		{"dirty", 0, 8, true}, {"clean", 8, 8, true},
		{"adjacent", 0, 16, true}, {"interior", 3, 10, true},
		{"cold", 16, 8, false}, {"cold_tail", 8, 9, false},
		{"cold_head", 23, 9, false}, {"cold_middle", 0, 32, false},
		{"gap", 24, 24, false}, {"hole", 32, 8, false},
		{"island", 40, 8, true}, {"past_end", 40, 9, false},
		{"empty", 48, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.IsRangeResident(ctx, "f", tc.off, tc.n)
			if err != nil || got != tc.want {
				t.Fatalf("resident(%d,%d)=(%v,%v), want %v", tc.off, tc.n, got, err, tc.want)
			}
		})
	}
	if got, err := s.IsRangeResident(ctx, "missing", 0, 1); err != nil || got {
		t.Fatalf("missing=(%v,%v)", got, err)
	}
	// A newer write supersedes part of the cold range without making its
	// remaining cold bytes resident.
	if err := s.WriteAt(ctx, "f", 18, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if got, err := s.IsRangeResident(ctx, "f", 18, 3); err != nil || !got {
		t.Fatalf("overwritten cold bytes=(%v,%v)", got, err)
	}
	if got, err := s.IsRangeResident(ctx, "f", 16, 8); err != nil || got {
		t.Fatalf("partially overwritten cold range=(%v,%v)", got, err)
	}
	if err := s.Truncate(ctx, "f", 28); err != nil {
		t.Fatal(err)
	}
	if got, err := s.IsRangeResident(ctx, "f", 24, 8); err != nil || got {
		t.Fatalf("truncated range=(%v,%v)", got, err)
	}
	if err := s.Delete(ctx, "f"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.IsRangeResident(ctx, "f", 0, 1); err != nil || got {
		t.Fatalf("deleted=(%v,%v)", got, err)
	}
}

func TestIsRangeResidentErrorsAndAllocations(t *testing.T) {
	s := testStore(t, Config{})
	ctx := context.Background()
	for _, r := range [][2]int64{{-1, 1}, {0, -1}, {math.MaxInt64, 1}, {1, math.MaxInt64}} {
		if got, err := s.IsRangeResident(ctx, "f", r[0], r[1]); err == nil || got {
			t.Fatalf("invalid range %v=(%v,%v)", r, got, err)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if got, err := s.IsRangeResident(cancelled, "f", 0, 0); !errors.Is(err, context.Canceled) || got {
		t.Fatalf("cancelled=(%v,%v)", got, err)
	}
	if err := s.WriteAt(ctx, "f", 0, make([]byte, 4096)); err != nil {
		t.Fatal(err)
	}
	if n := testing.AllocsPerRun(100, func() {
		got, err := s.IsRangeResident(ctx, "f", 0, 4096)
		if err != nil || !got {
			t.Fatalf("resident=(%v,%v)", got, err)
		}
	}); n != 0 {
		t.Fatalf("resident query allocated %g times, want zero", n)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := s.IsRangeResident(ctx, "f", 0, 4096); !errors.Is(err, errClosed) || got {
		t.Fatalf("closed=(%v,%v)", got, err)
	}
}

// The payload bytes are deliberately absent: residency inspects only the
// interval index. Queries near its tail make an accidental full scan visible.
func BenchmarkIsRangeResident(b *testing.B) {
	for _, n := range []int{1024, 100000, 1000000} {
		for _, covered := range []int{1, 256} {
			b.Run(fmt.Sprintf("intervals=%d/covered=%d", n, covered), func(b *testing.B) {
				s, err := openJournal(b.TempDir(), Config{ShardCount: 1})
				if err != nil {
					b.Fatal(err)
				}
				b.Cleanup(func() { _ = s.Close() })
				fi := &fileIndex{ivs: make([]interval, n)}
				for i := range fi.ivs {
					fi.ivs[i] = interval{fileOff: int64(i) * 4096, length: 4096}
				}
				sh := s.shardFor("f")
				sh.mu.Lock()
				sh.index["f"] = fi
				sh.mu.Unlock()
				off := int64(n-covered) * 4096
				ctx := context.Background()
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					resident, err := s.IsRangeResident(ctx, "f", off, int64(covered)*4096)
					if err != nil || !resident {
						b.Fatalf("resident=(%v,%v)", resident, err)
					}
				}
			})
		}
	}
}
