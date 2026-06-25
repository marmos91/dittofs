package engine

import (
	"context"
	"testing"
	"time"
)

// These tests exercise the adaptive control glue end-to-end through
// adaptiveUploadTick: the bytes counter the upload path increments + the
// limiter's peak in-flight → goodput + window-limited flag →
// goodputController.observe → uploadLimiter.SetLimit. They drive the limiter the
// way mirrorOnce does (Acquire/Release) so the window-limited vs app-limited
// distinction is real, then run a tick — pinning the integration without a clock.

func newAdaptiveTestSyncer() *Syncer {
	return &Syncer{
		uploadLimiter:    newDynamicSemaphore(AdaptiveUploadFloor),
		uploadController: newGoodputController(AdaptiveUploadFloor, AdaptiveUploadCeiling),
	}
}

// runTick simulates one control interval: `held` uploads run and complete
// within it (acquire then release sets the limiter's peak high-water, so the
// window-limited signal is real), delivering `bytes` with an optional error.
// The uploads have finished by the time the controller samples, exactly as in
// production, so in-flight is back to zero at tick time.
func runTick(t *testing.T, m *Syncer, held int, bytes int64, errFlag bool) int {
	t.Helper()
	ctx := context.Background()
	for j := 0; j < held; j++ {
		if err := m.uploadLimiter.Acquire(ctx); err != nil {
			t.Fatalf("acquire: %v", err)
		}
	}
	for j := 0; j < held; j++ {
		m.uploadLimiter.Release()
	}
	m.uploadedBytesWindow.Store(bytes)
	if errFlag {
		m.uploadErrWindow.Store(1)
	}
	w, _, _, _, _ := m.adaptiveUploadTick(time.Second)
	return w
}

func TestAdaptiveUploadTick_RampsWindowAsGoodputRises(t *testing.T) {
	m := newAdaptiveTestSyncer()
	const perConn = 2 * 1024 * 1024 // 2 MiB/s/conn — unsaturated link

	start := m.uploadLimiter.Limit()
	// Each interval fully saturates the window and delivers goodput proportional
	// to it (link not saturated), so the controller must keep opening the window.
	for i := 0; i < 6; i++ {
		w := m.uploadLimiter.Limit()
		runTick(t, m, w, int64(w*perConn), false)
	}
	if m.uploadLimiter.Limit() <= start {
		t.Fatalf("window did not ramp on rising goodput: start=%d end=%d", start, m.uploadLimiter.Limit())
	}
}

func TestAdaptiveUploadTick_SettlesWhenLinkSaturates(t *testing.T) {
	m := newAdaptiveTestSyncer()
	const perConn = 2 * 1024 * 1024
	const saturationConns = 40 // goodput stops rising past this many connections (above the floor)

	var last int
	for i := 0; i < 20; i++ {
		w := m.uploadLimiter.Limit()
		effective := w
		if effective > saturationConns {
			effective = saturationConns
		}
		// Window fully saturated, but goodput plateaus past the knee.
		last = runTick(t, m, w, int64(effective*perConn), false)
	}
	if last >= AdaptiveUploadCeiling {
		t.Fatalf("window pinned at ceiling on a saturated link: %d", last)
	}
	if last <= AdaptiveUploadFloor {
		t.Fatalf("window collapsed to the floor despite useful goodput: %d", last)
	}
}

func TestAdaptiveUploadTick_HoldsWindowWhenPipelineStarves(t *testing.T) {
	m := newAdaptiveTestSyncer()
	const perConn = 2 * 1024 * 1024
	// Ramp up while window-limited.
	for i := 0; i < 5; i++ {
		w := m.uploadLimiter.Limit()
		runTick(t, m, w, int64(w*perConn), false)
	}
	high := m.uploadLimiter.Limit()
	if high <= AdaptiveUploadFloor {
		t.Fatalf("precondition: expected ramp, got %d", high)
	}
	// Upstream pipeline starves: only 2 chunks ever in flight (peak=2 << window),
	// goodput tiny. This is app-limited, not congestion — the window MUST hold so
	// the drain is fast when the backlog bursts (the #1407 collapse bug).
	for i := 0; i < 6; i++ {
		w := runTick(t, m, 2, 2*perConn, false)
		if w != high {
			t.Fatalf("starve sample %d moved the window: %d -> %d", i, high, w)
		}
	}
}

func TestAdaptiveUploadTick_SkipsIdleInterval(t *testing.T) {
	m := newAdaptiveTestSyncer()
	// Saturate + deliver bytes so the first tick acts.
	runTick(t, m, m.uploadLimiter.Limit(), 64*1024*1024, false)
	before := m.uploadLimiter.Limit()

	// Idle interval: no bytes, nothing in flight, no error. Must not act.
	_, _, _, _, acted := m.adaptiveUploadTick(time.Second)
	if acted {
		t.Fatal("adaptiveUploadTick acted on an idle interval")
	}
	if m.uploadLimiter.Limit() != before {
		t.Fatalf("idle interval changed the window: %d -> %d", before, m.uploadLimiter.Limit())
	}
}

func TestAdaptiveUploadTick_BacksOffOnError(t *testing.T) {
	m := newAdaptiveTestSyncer()
	for i := 0; i < 4; i++ {
		w := m.uploadLimiter.Limit()
		runTick(t, m, w, int64(w)*4*1024*1024, false)
	}
	high := m.uploadLimiter.Limit()
	if high <= AdaptiveUploadFloor {
		t.Fatalf("precondition: expected ramp above floor, got %d", high)
	}
	// An upload error in the interval must shrink the window.
	after := runTick(t, m, high, int64(high)*4*1024*1024, true)
	if after >= high {
		t.Fatalf("window did not back off after an upload error: %d -> %d", high, after)
	}
}
