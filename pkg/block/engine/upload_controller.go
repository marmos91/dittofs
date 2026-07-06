package engine

import "math"

// goodputController is the pure decision core of adaptive upload concurrency
// (#1407). When the user does not pin --parallel-uploads, the syncer ramps the
// number of concurrent CAS-chunk uploads to saturate the uplink on its own.
//
// The control signal is GOODPUT (delivered bytes/sec), not latency. An earlier
// latency-gradient design (#1400) read the per-PUT latency rise *caused by
// useful concurrency* as congestion and collapsed the window to ~4, far below
// the bandwidth knee. Uploads here are network-latency bound, so opening more
// connections raises both latency and throughput together until the link
// saturates — the only honest saturation signal is "did goodput stop rising".
//
// The loop is TCP-slow-start-like: start at a floor, ramp multiplicatively
// while goodput keeps improving, hold when it plateaus, settle at the window
// that delivered the best goodput (the knee), and back off only on upload
// errors or a goodput collapse. It is deterministic and depends on no clock,
// network, or goroutine, so its behaviour is pinned entirely by unit tests.
type goodputController struct {
	floor   int
	ceiling int

	cur        int     // current target concurrency
	bestWindow int     // window that delivered bestGoodput (the knee)
	best       float64 // best smoothed goodput seen so far
	ema        float64 // EWMA-smoothed goodput
	emaInit    bool
	stall      int // consecutive samples without a meaningful improvement

	// Tunables. Defaults chosen for the S3 upload path (see newGoodputController).
	rampFactor    float64 // multiplicative window increase while improving
	backoffFactor float64 // multiplicative decrease on error / collapse
	improveFrac   float64 // min relative goodput gain that counts as "improving"
	collapseFrac  float64 // goodput below best*collapseFrac is treated as collapse
	emaAlpha      float64 // EWMA weight on the newest sample
	stallLimit    int     // plateau samples before settling at the knee
}

// newGoodputController returns a controller that ramps within [floor, ceiling].
func newGoodputController(floor, ceiling int) *goodputController {
	if floor < 1 {
		floor = 1
	}
	if ceiling < floor {
		ceiling = floor
	}
	return &goodputController{
		floor:         floor,
		ceiling:       ceiling,
		cur:           floor,
		bestWindow:    floor,
		rampFactor:    1.5,
		backoffFactor: 0.7,
		improveFrac:   0.10,
		collapseFrac:  0.5,
		emaAlpha:      0.5,
		stallLimit:    3,
	}
}

// window returns the current target concurrency.
func (c *goodputController) window() int { return c.cur }

// observe feeds one control-interval sample and returns the next target window.
// goodput is the delivered bytes/sec over the interval; windowLimited is true
// when the window was actually saturated during the interval (in-flight uploads
// reached the limit); sawError is true if any upload failed.
//
// windowLimited is the crux. Goodput only carries information about the window
// when the window is the binding constraint. When it is NOT — the upstream
// rollup pipeline produced fewer chunks than the window could carry — goodput
// is app-limited, and reacting to it would shrink the window precisely when the
// pipeline is about to burst a backlog that a wide window must drain fast. The
// bursty rollup→upload pipeline does exactly this, so the controller only ramps
// or backs off on window-limited samples and otherwise holds (#1407).
func (c *goodputController) observe(goodput float64, windowLimited, sawError bool) int {
	// Smooth the (noisy) per-interval goodput before any decision.
	if !c.emaInit {
		c.ema = goodput
		c.emaInit = true
	} else {
		c.ema = c.emaAlpha*goodput + (1-c.emaAlpha)*c.ema
	}

	switch {
	case sawError && windowLimited:
		// Server pushback / overload while the window was the binding constraint.
		// Shrink and re-probe later. Decay the best reference too so recovery is
		// judged against the post-backoff regime rather than an unreachable peak.
		c.shrink()
		c.best *= c.backoffFactor

	case !windowLimited:
		// App-limited: the upstream pipeline, not the window, capped goodput.
		// Hold the window — there is no congestion signal, and shrinking would
		// throttle the drain the moment upstream catches up. A stray error here
		// (e.g. one chunk failing during a low-trickle phase) is NOT treated as
		// congestion: the window had nothing to do with it, so corrupting the
		// learned knee over it would needlessly deflate the next burst.
		return c.cur

	case c.best > 0 && goodput < c.best*c.collapseFrac:
		// Goodput fell sharply below the best we have seen with no explicit
		// error — treat as congestion collapse and back off. This branch reads
		// the RAW sample, not the EWMA: a real collapse must be reacted to
		// immediately (AIMD-style), whereas the improvement branch below stays
		// smoothed so transient noise does not drive the window up.
		c.shrink()

	case c.ema > c.best*(1+c.improveFrac):
		// Still climbing: record the knee-so-far and open the window further.
		c.best = c.ema
		c.bestWindow = c.cur
		c.stall = 0
		c.grow()

	default:
		// Plateau: goodput is not meaningfully better than the best seen. Hold,
		// and after enough flat samples settle back at the knee (the smallest
		// window that delivered near-peak goodput — less resource use for the
		// same throughput).
		if c.ema > c.best {
			c.best = c.ema
		}
		c.stall++
		if c.stall >= c.stallLimit {
			c.cur = c.bestWindow
		}
	}
	return c.cur
}

func (c *goodputController) grow() {
	next := int(math.Ceil(float64(c.cur) * c.rampFactor))
	if next <= c.cur {
		next = c.cur + 1
	}
	if next > c.ceiling {
		next = c.ceiling
	}
	c.cur = next
}

func (c *goodputController) shrink() {
	next := int(math.Round(float64(c.cur) * c.backoffFactor))
	if next >= c.cur {
		next = c.cur - 1
	}
	if next < c.floor {
		next = c.floor
	}
	c.cur = next
	c.bestWindow = next
	c.stall = 0
}
