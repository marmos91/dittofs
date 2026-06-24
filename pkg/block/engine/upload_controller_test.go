package engine

import "testing"

// The goodput controller is the pure decision core of adaptive upload
// concurrency (#1407). It consumes one (goodput, sawError) sample per control
// interval and returns the next target window. It must:
//   - start at the floor,
//   - ramp up while delivered goodput keeps improving (TCP-slow-start-like),
//   - settle at the knee once goodput stops improving (no endless growth),
//   - back off on upload errors or a goodput collapse,
//   - never leave [floor, ceiling].
//
// These tests pin the algorithm independently of any network, goroutine, or
// clock, so a regression in the math is caught here rather than only on the VM.

func TestGoodputController_StartsAtFloor(t *testing.T) {
	c := newGoodputController(8, 64)
	if got := c.window(); got != 8 {
		t.Fatalf("initial window = %d, want floor 8", got)
	}
}

func TestGoodputController_RampsUpWhileGoodputImproves(t *testing.T) {
	c := newGoodputController(8, 64)
	// Each wider window delivers strictly more goodput: the link is not yet
	// saturated, so the controller must keep opening the window.
	prev := c.window()
	goodput := 10.0
	for i := 0; i < 8; i++ {
		w := c.observe(goodput, true, false)
		if w < prev {
			t.Fatalf("sample %d: window shrank (%d -> %d) while goodput rising", i, prev, w)
		}
		prev = w
		goodput *= 2 // keep improving well above the noise threshold
		if w >= 64 {
			break // reached the ceiling; cannot grow further
		}
	}
	if prev <= 8 {
		t.Fatalf("window stayed at floor despite rising goodput: %d", prev)
	}
}

func TestGoodputController_SettlesAtKneeWhenGoodputPlateaus(t *testing.T) {
	c := newGoodputController(8, 64)
	// Phase 1: goodput climbs, window ramps.
	c.observe(10, true, false)
	c.observe(20, true, false)
	c.observe(30, true, false)
	peak := c.window()
	// Phase 2: goodput flat (link saturated). The window must stop growing and
	// converge — it must NOT keep climbing to the ceiling on a saturated link.
	var last int
	for i := 0; i < 8; i++ {
		last = c.observe(30, true, false)
	}
	if last > peak {
		t.Fatalf("window kept growing past the knee on flat goodput: peak=%d last=%d", peak, last)
	}
	// And it must have converged (a second identical run of samples is stable).
	stable := c.observe(30, true, false)
	if stable != last {
		t.Fatalf("window not converged on flat goodput: %d then %d", last, stable)
	}
}

func TestGoodputController_DoesNotExceedCeiling(t *testing.T) {
	c := newGoodputController(8, 32)
	// Goodput improves forever; only the ceiling should stop the ramp.
	goodput := 10.0
	for i := 0; i < 20; i++ {
		w := c.observe(goodput, true, false)
		if w > 32 {
			t.Fatalf("window exceeded ceiling: %d", w)
		}
		goodput *= 1.5
	}
	if c.window() != 32 {
		t.Fatalf("window did not reach ceiling under unbounded goodput: %d", c.window())
	}
}

func TestGoodputController_BacksOffOnError(t *testing.T) {
	c := newGoodputController(8, 64)
	// Ramp up first.
	c.observe(10, true, false)
	c.observe(20, true, false)
	c.observe(40, true, false)
	high := c.window()
	if high <= 8 {
		t.Fatalf("precondition: window should have ramped, got %d", high)
	}
	// An upload error must shrink the window (server pushback / overload).
	after := c.observe(40, true, true)
	if after >= high {
		t.Fatalf("window did not back off on error: %d -> %d", high, after)
	}
}

func TestGoodputController_BacksOffOnGoodputCollapse(t *testing.T) {
	c := newGoodputController(8, 64)
	c.observe(10, true, false)
	c.observe(40, true, false)
	c.observe(80, true, false)
	high := c.window()
	// Goodput collapses to a fraction of the best seen (congestion), no explicit
	// error. The controller must still shrink.
	after := c.observe(10, true, false)
	if after >= high {
		t.Fatalf("window did not back off on goodput collapse: %d -> %d", high, after)
	}
}

func TestGoodputController_NeverBelowFloor(t *testing.T) {
	c := newGoodputController(8, 64)
	c.observe(50, true, false)
	// Hammer it with errors; it must never drop below the floor.
	for i := 0; i < 20; i++ {
		w := c.observe(1, true, true)
		if w < 8 {
			t.Fatalf("window dropped below floor: %d", w)
		}
	}
}

func TestGoodputController_HoldsWindowWhenAppLimited(t *testing.T) {
	c := newGoodputController(8, 64)
	// Ramp up while window-limited.
	c.observe(10, true, false)
	c.observe(20, true, false)
	c.observe(40, true, false)
	high := c.window()
	if high <= 8 {
		t.Fatalf("precondition: expected ramp, got %d", high)
	}
	// Now the upstream pipeline starves the uploader: goodput collapses but the
	// window was NOT the constraint (app-limited). The controller must HOLD the
	// window — shrinking here would cripple throughput the moment upstream
	// catches up (the bursty rollup→upload pipeline does exactly this, #1407).
	for i := 0; i < 5; i++ {
		w := c.observe(1, false, false) // low goodput, app-limited
		if w != high {
			t.Fatalf("app-limited sample %d moved the window: %d -> %d", i, high, w)
		}
	}
}

func TestGoodputController_HoldsOnErrorWhenAppLimited(t *testing.T) {
	c := newGoodputController(8, 64)
	// Ramp up while window-limited.
	c.observe(10, true, false)
	c.observe(20, true, false)
	c.observe(40, true, false)
	high := c.window()
	if high <= 8 {
		t.Fatalf("precondition: expected ramp, got %d", high)
	}
	// A stray error during an app-limited interval (window was NOT the
	// constraint) must NOT shrink the window or corrupt the learned knee — that
	// would needlessly deflate the next burst.
	after := c.observe(1, false, true)
	if after != high {
		t.Fatalf("app-limited error moved the window: %d -> %d", high, after)
	}
}

func TestGoodputController_RecoversAfterBackoff(t *testing.T) {
	c := newGoodputController(8, 64)
	c.observe(10, true, false)
	c.observe(20, true, false)
	c.observe(40, true, false)
	c.observe(40, true, true) // back off
	low := c.window()
	// Conditions recover: goodput climbs again, window must re-open.
	c.observe(80, true, false)
	c.observe(160, true, false)
	if c.window() <= low {
		t.Fatalf("window did not recover after backoff: low=%d now=%d", low, c.window())
	}
}
