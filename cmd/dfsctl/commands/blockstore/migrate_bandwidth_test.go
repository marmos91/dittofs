package blockstore

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestParseBandwidthLimit covers the 11 unit cases from the plan
// (behaviors 1–11). 0/empty mean unlimited; SI suffix is 1000-base;
// IEC suffix is 1024-base; negative or garbage returns
// ErrInvalidBandwidth.
func TestParseBandwidthLimit(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"", 0, false},
		{"0", 0, false},
		{"100", 100, false},
		{"50KB", 50_000, false},
		{"50KiB", 51_200, false},
		{"100MB", 100_000_000, false},
		{"100MiB", 104_857_600, false},
		{"1GB", 1_000_000_000, false},
		{"1GiB", 1_073_741_824, false},
		{"garbage", 0, true},
		{"-50MB", 0, true},
	}
	for _, tc := range cases {
		got, err := ParseBandwidthLimit(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseBandwidthLimit(%q) err=nil, want non-nil", tc.in)
				continue
			}
			if !errors.Is(err, ErrInvalidBandwidth) {
				t.Errorf("ParseBandwidthLimit(%q) err=%v, want wrap of ErrInvalidBandwidth", tc.in, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseBandwidthLimit(%q) err=%v, want nil", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseBandwidthLimit(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestParseBandwidthLimit_LowercaseSuffix verifies that the unit
// portion is case-insensitive ("kb" == "KB", "mib" == "MiB", etc.).
func TestParseBandwidthLimit_LowercaseSuffix(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"50kb", 50_000},
		{"50kib", 51_200},
		{"100mb", 100_000_000},
		{"100mib", 104_857_600},
		{"1gb", 1_000_000_000},
		{"1gib", 1_073_741_824},
	}
	for _, tc := range cases {
		got, err := ParseBandwidthLimit(tc.in)
		if err != nil {
			t.Errorf("ParseBandwidthLimit(%q) err=%v, want nil", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseBandwidthLimit(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestBandwidthWait_Unlimited (Test 13): nil limiter returns nil
// immediately even for huge n.
func TestBandwidthWait_Unlimited(t *testing.T) {
	start := time.Now()
	if err := bandwidthWait(context.Background(), nil, 1<<30); err != nil {
		t.Fatalf("bandwidthWait(nil, 1GB) err=%v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("bandwidthWait(nil, ...) took %v; want near-zero", elapsed)
	}
}

// TestBandwidthWait_LimitsRate (Test 12, adjusted for 1 MiB burst
// floor): with limit=4 MiB/s and two 4-MiB requests, the second WaitN
// must block ~1s so total wall-clock is >= 700ms.
//
// Rationale: the plan's literal "4 KB across 2 chunks at 1000 B/s →
// >=4s" example is incompatible with the 1 MiB burst floor (4 KB <
// burst → first call returns instantly, second call returns instantly
// because the bucket refills well within 4 KB worth of time). The
// floor exists so 16 MiB FastCDC chunks don't trip rate.Limiter's
// "burst exceeded" rejection. Picking a request size > burst is the
// honest way to assert the limiter is enforcing the rate.
func TestBandwidthWait_LimitsRate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping wall-clock-bound test in -short")
	}
	const bps = 4 * 1024 * 1024 // 4 MiB/s
	limiter := newBandwidthLimiter(bps)
	if limiter == nil {
		t.Fatal("newBandwidthLimiter returned nil")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// First 4 MiB drains the bucket (burst = bps = 4 MiB → 1s window).
	// Second 4 MiB must wait ~1s for refill. Total >=700ms allowing CI
	// tolerance.
	start := time.Now()
	for i := 0; i < 2; i++ {
		if err := bandwidthWait(ctx, limiter, 4*1024*1024); err != nil {
			t.Fatalf("bandwidthWait[%d] err=%v", i, err)
		}
	}
	elapsed := time.Since(start)
	if elapsed < 700*time.Millisecond {
		t.Errorf("two 4 MiB waits at 4 MiB/s took %v; want >=700ms", elapsed)
	}
}

// TestNewBandwidthLimiter_ZeroIsUnlimited verifies the contract:
// bps<=0 returns nil so callers must nil-check.
func TestNewBandwidthLimiter_ZeroIsUnlimited(t *testing.T) {
	if l := newBandwidthLimiter(0); l != nil {
		t.Errorf("newBandwidthLimiter(0) = %v, want nil", l)
	}
	if l := newBandwidthLimiter(-1); l != nil {
		t.Errorf("newBandwidthLimiter(-1) = %v, want nil", l)
	}
}

// TestBandwidthWait_SplitOversizedRequest verifies a request larger
// than the limiter's burst is split across multiple WaitN calls
// (rate.Limiter.WaitN refuses n > burst). Burst is floored at 1 MB; we
// pass an 8 MB request under a 100 KB/s limit (burst floor active).
func TestBandwidthWait_SplitOversizedRequest(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping wall-clock-bound test in -short")
	}
	// 100 KB/s -> burst = 1 MB (floor).
	limiter := newBandwidthLimiter(100 * 1000)
	if limiter == nil {
		t.Fatal("newBandwidthLimiter(100*1000) returned nil")
	}
	// 1.5 MB request: must succeed by splitting across burst windows.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := bandwidthWait(ctx, limiter, 3*512*1024); err != nil {
		t.Fatalf("bandwidthWait split-oversize err=%v", err)
	}
}
