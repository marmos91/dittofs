package memory

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
)

func TestIsRangeResident(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()
	if err := s.WriteAt(ctx, "f", 2, []byte("ab")); err != nil {
		t.Fatal(err)
	}
	if err := s.Hydrate(ctx, "f", 4, []byte("cd"), 0); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteAt(ctx, "f", 8, []byte("ef")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		off, n int64
		want   bool
	}{{2, 4, true}, {3, 2, true}, {0, 4, false}, {2, 8, false}, {8, 2, true}, {10, 1, false}, {10, 0, true}} {
		got, err := s.IsRangeResident(ctx, "f", tc.off, tc.n)
		if err != nil || got != tc.want {
			t.Fatalf("resident(%d,%d)=(%v,%v), want %v", tc.off, tc.n, got, err, tc.want)
		}
	}
	if got, err := s.IsRangeResident(ctx, "missing", 0, 1); err != nil || got {
		t.Fatalf("missing=(%v,%v)", got, err)
	}
	for _, r := range [][2]int64{{-1, 1}, {0, -1}, {math.MaxInt64, 1}} {
		if got, err := s.IsRangeResident(ctx, "f", r[0], r[1]); err == nil || got {
			t.Fatalf("invalid range %v=(%v,%v)", r, got, err)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if got, err := s.IsRangeResident(cancelled, "f", 2, 4); !errors.Is(err, context.Canceled) || got {
		t.Fatalf("cancelled=(%v,%v)", got, err)
	}
	if err := s.Truncate(ctx, "f", 4); err != nil {
		t.Fatal(err)
	}
	if got, err := s.IsRangeResident(ctx, "f", 2, 4); err != nil || got {
		t.Fatalf("truncated=(%v,%v)", got, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := s.IsRangeResident(ctx, "f", 2, 2); !errors.Is(err, block.ErrStoreClosed) || got {
		t.Fatalf("closed=(%v,%v)", got, err)
	}
}
