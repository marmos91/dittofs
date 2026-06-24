package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

const mib = 1 << 20

// fakeClock returns a now() func backed by a mutable timestamp.
func fakeClock(t *time.Time) func() time.Time {
	return func() time.Time { return *t }
}

func TestUploadLimiter_FixedMode(t *testing.T) {
	l := NewUploadLimiter(4)
	if l.adaptive {
		t.Fatal("fixedN>0 must produce a non-adaptive limiter")
	}
	if got := l.Limit(); got != 4 {
		t.Fatalf("Limit()=%d, want 4", got)
	}
	// Observe must not move a fixed window — not on success, not on error.
	for i := 0; i < 100; i++ {
		l.Observe(mib, 10*time.Millisecond, nil)
	}
	l.Observe(mib, time.Second, errors.New("boom"))
	if got := l.Limit(); got != 4 {
		t.Fatalf("fixed Limit changed to %d after Observe; want 4", got)
	}
}

func TestUploadLimiter_AdaptiveStartsAtInitialWindow(t *testing.T) {
	l := NewUploadLimiter(0)
	if !l.adaptive {
		t.Fatal("fixedN<=0 must produce an adaptive limiter")
	}
	if l.cap != UploadSafetyRail {
		t.Fatalf("adaptive cap=%d, want %d", l.cap, UploadSafetyRail)
	}
	if got := l.Limit(); got != adaptiveInitialWindow {
		t.Fatalf("initial Limit()=%d, want %d", got, adaptiveInitialWindow)
	}
}

// With no latency inflation (rtt == baseRTT) and no goodput sampling yet
// (clock frozen), the window must ramp upward.
func TestUploadLimiter_AdaptiveGrowsWhenUncongested(t *testing.T) {
	now := time.Unix(0, 0)
	l := NewUploadLimiter(0)
	l.now = fakeClock(&now)

	start := l.window
	for i := 0; i < 50; i++ {
		l.Observe(mib, 100*time.Millisecond, nil)
	}
	if l.window <= start {
		t.Fatalf("window did not grow: start=%.2f end=%.2f", start, l.window)
	}
}

// Latency inflation above baseRTT must shrink the window.
func TestUploadLimiter_AdaptiveShrinksWhenCongested(t *testing.T) {
	now := time.Unix(0, 0)
	l := NewUploadLimiter(0)
	l.now = fakeClock(&now)

	// Establish a fast propagation floor.
	l.Observe(mib, 10*time.Millisecond, nil)
	before := l.window
	// Now every PUT is 10x slower → queueing → shrink.
	for i := 0; i < 20; i++ {
		l.Observe(mib, 100*time.Millisecond, nil)
	}
	if l.window >= before {
		t.Fatalf("window did not shrink under congestion: before=%.2f after=%.2f", before, l.window)
	}
}

func TestUploadLimiter_ErrorBacksOff(t *testing.T) {
	l := NewUploadLimiter(0)
	// Grow a little first.
	for i := 0; i < 10; i++ {
		l.Observe(mib, 50*time.Millisecond, nil)
	}
	before := l.window
	l.Observe(mib, 0, errors.New("503 slow down"))
	if l.window >= before {
		t.Fatalf("error did not back off: before=%.2f after=%.2f", before, l.window)
	}
	if l.window < 1 {
		t.Fatalf("backoff went below floor: %.2f", l.window)
	}
}

// With the clock frozen the goodput gate never samples, so an uncongested
// window must ramp all the way to the safety rail and stop there.
func TestUploadLimiter_RampsToSafetyRail(t *testing.T) {
	now := time.Unix(0, 0)
	l := NewUploadLimiter(0)
	l.now = fakeClock(&now)
	for i := 0; i < 200000; i++ {
		l.Observe(mib, 100*time.Millisecond, nil)
	}
	if got := l.Limit(); got != UploadSafetyRail {
		t.Fatalf("Limit()=%d, want safety rail %d", got, UploadSafetyRail)
	}
	if l.window > float64(UploadSafetyRail)+1e-9 {
		t.Fatalf("window %.2f exceeded safety rail %d", l.window, UploadSafetyRail)
	}
}

// When aggregate goodput plateaus, the gate must stop the window from growing
// past the throughput-peak watermark even though latency stays at baseRTT.
func TestUploadLimiter_GoodputGateHaltsGrowth(t *testing.T) {
	now := time.Unix(0, 0)
	l := NewUploadLimiter(0)
	l.now = fakeClock(&now)

	// Tick the controller once per 600ms (> goodputSampleInterval) with a
	// constant per-tick delivery and constant (floor) latency → flat goodput.
	tick := func() {
		now = now.Add(600 * time.Millisecond)
		l.Observe(mib, 100*time.Millisecond, nil)
	}
	// Prime the gate: the first Observe only seeds lastSample; the second
	// (one interval later) fires the first sample, setting peak + windowAtPeak.
	tick()
	tick()
	peakWindow := l.windowAtPeak
	if peakWindow == 0 {
		t.Fatal("expected windowAtPeak to be set after first sample")
	}
	// Many more flat-goodput ticks must not push the window past the peak.
	for i := 0; i < 100; i++ {
		tick()
	}
	if l.window > peakWindow+1e-9 {
		t.Fatalf("window grew past peak under flat goodput: peak=%.2f window=%.2f", peakWindow, l.window)
	}
}

// Regression for the goodput-gate bug (#1400 review): while aggregate goodput
// keeps RISING sample-over-sample, the window must keep ramping — it must not
// freeze at the first sample's window. The original gate clamped on every
// sample because it compared goodput against a peak it had just overwritten.
func TestUploadLimiter_AdaptiveRampsWhileGoodputRises(t *testing.T) {
	now := time.Unix(0, 0)
	l := NewUploadLimiter(0)
	l.now = fakeClock(&now)

	// Establish the propagation floor (no sample yet).
	l.Observe(mib, 100*time.Millisecond, nil)

	var winAfterFirstSample float64
	bytesPerTick := 4 * mib
	for i := 0; i < 12; i++ {
		now = now.Add(goodputSampleInterval + 10*time.Millisecond)
		l.Observe(bytesPerTick, 100*time.Millisecond, nil) // latency stays at floor
		if i == 0 {
			winAfterFirstSample = l.window
		}
		bytesPerTick += 4 * mib // strictly rising goodput
	}
	if l.window <= winAfterFirstSample {
		t.Fatalf("window did not ramp under rising goodput: first-sample=%.2f end=%.2f (gate froze growth)",
			winAfterFirstSample, l.window)
	}
}

func TestUploadLimiter_AcquireBlocksAtWindow(t *testing.T) {
	l := NewUploadLimiter(2)
	ctx := context.Background()
	if err := l.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
	if err := l.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
	// Third Acquire must block until a slot frees.
	done := make(chan struct{})
	go func() {
		_ = l.Acquire(ctx)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("third Acquire returned while window was full")
	case <-time.After(50 * time.Millisecond):
	}
	l.ReleaseSlot()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Acquire did not unblock after ReleaseSlot")
	}
	l.ReleaseSlot()
	l.ReleaseSlot()
}

func TestUploadLimiter_AcquireHonorsContext(t *testing.T) {
	l := NewUploadLimiter(1)
	ctx := context.Background()
	if err := l.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(ctx)
	errCh := make(chan error, 1)
	go func() { errCh <- l.Acquire(cctx) }()
	// Let it park on the full window, then cancel.
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Acquire err=%v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled Acquire did not return")
	}
	l.ReleaseSlot()
}

// Race/stress: many goroutines Acquire/Observe/ReleaseSlot concurrently. Run
// under -race. A fixed window pins cap == window == fixedN, so the assertion
// tests the exact dispatch invariant (in-flight never exceeds the window),
// not just the loose safety rail.
func TestUploadLimiter_ConcurrentNoOverDispatch(t *testing.T) {
	const fixedN = 8
	l := NewUploadLimiter(fixedN)
	ctx := context.Background()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var live, maxLive int

	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if err := l.Acquire(ctx); err != nil {
					return
				}
				mu.Lock()
				live++
				if live > maxLive {
					maxLive = live
				}
				mu.Unlock()
				l.Observe(mib, time.Millisecond, nil)
				mu.Lock()
				live--
				mu.Unlock()
				l.ReleaseSlot()
			}
		}()
	}
	wg.Wait()
	if maxLive > fixedN {
		t.Fatalf("max concurrent in-flight %d exceeded window %d", maxLive, fixedN)
	}
}
