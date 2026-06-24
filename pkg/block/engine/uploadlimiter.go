package engine

import (
	"context"
	"math"
	"sync"
	"time"
)

// UploadLimiter bounds the number of concurrent CAS-chunk uploads to one
// remote store. It runs in one of two modes:
//
//   - Fixed: an operator pinned the ceiling via --parallel-uploads N. The
//     window is constant N; the controller is inert. This reproduces the
//     static bounded-concurrency behaviour of #1397.
//
//   - Adaptive: no operator ceiling. The window ramps at runtime toward the
//     uplink's bandwidth knee, bounded only by UploadSafetyRail. Uploads are
//     network-bound, not CPU-bound (#1266 measured 5.5% CPU at saturation),
//     so the CPU-deduced default is the wrong shape; the limiter discovers
//     the right in-flight count from the link itself.
//
// The adaptive controller is a hybrid (issue #1398): a latency-gradient
// (Vegas/Gradient2) drives the window — per-PUT latency inflates precisely
// when the uplink queue fills, so baseRTT/rtt locates the knee without
// perturbation — and an aggregate-goodput gate caps growth, refusing to
// expand past the window that achieved peak throughput when delivered MB/s
// has stopped rising (a bufferbloat guard, since the gradient's sqrt headroom
// would otherwise keep probing upward forever). Upload errors (e.g. 503
// SlowDown, timeouts) are a rare hard-safety path: multiplicative decrease
// only. They never drive the steady state — the bottleneck is uplink
// bandwidth, which saturates far below S3's request-rate limit.
//
// One limiter is shared across all shares that target the same remote
// (the runtime keys it by remote config ID alongside the ref-counted store)
// so they cooperate on a single bandwidth estimate instead of each ramping
// independently. All methods are safe for concurrent use.
type UploadLimiter struct {
	mu   sync.Mutex
	cond *sync.Cond
	now  func() time.Time // injectable for tests

	adaptive bool
	cap      int // hard ceiling: operator N, or UploadSafetyRail when adaptive

	window   float64 // congestion window (fractional for smooth AIMD)
	inflight int

	baseRTT time.Duration // min observed per-PUT latency (propagation floor)

	// Aggregate-goodput gate state.
	delivered    int64     // bytes acked since the last goodput sample
	lastSample   time.Time // wall clock of the last goodput sample
	sampledOnce  bool
	goodputEWMA  float64 // smoothed aggregate delivered bytes/sec
	peakGoodput  float64 // best goodput seen so far
	windowAtPeak float64 // window that achieved peakGoodput
	plateaued    bool    // a sample showed goodput stopped improving
}

const (
	// UploadSafetyRail bounds in-flight uploads in adaptive mode, guarding
	// against FD/goroutine exhaustion when no operator ceiling is set. Matches
	// the [0,256] validation bound on --parallel-uploads.
	UploadSafetyRail = 256

	// adaptiveInitialWindow is where an adaptive limiter starts before it has
	// measured the link; it ramps up from here.
	adaptiveInitialWindow = 8

	// windowSmoothing is the EWMA weight on each new window target. Lower =
	// steadier, slower to react.
	windowSmoothing = 0.2
	// minGradient floors baseRTT/rtt so a single latency spike cannot collapse
	// the window toward 1 in one step.
	minGradient = 0.5
	// goodputSampleInterval is the wall-clock window over which delivered bytes
	// are integrated into one aggregate-goodput sample.
	goodputSampleInterval = 500 * time.Millisecond
	// goodputSmoothing is the EWMA weight on each goodput sample.
	goodputSmoothing = 0.3
	// goodputGrowthEpsilon is the fractional goodput improvement required to
	// justify pushing the window past the throughput-peak watermark.
	goodputGrowthEpsilon = 0.05
	// errorBackoff multiplies the window on an upload error (multiplicative
	// decrease — the rare hard-safety path).
	errorBackoff = 0.8
)

// NewUploadLimiter builds a per-remote upload limiter. fixedN > 0 pins the
// window at fixedN (operator --parallel-uploads: a static cap, no ramp).
// fixedN <= 0 enables adaptive mode (ramp toward the measured knee, bounded
// by UploadSafetyRail).
func NewUploadLimiter(fixedN int) *UploadLimiter {
	l := &UploadLimiter{now: time.Now}
	if fixedN > 0 {
		l.adaptive = false
		l.cap = fixedN
		l.window = float64(fixedN)
	} else {
		l.adaptive = true
		l.cap = UploadSafetyRail
		l.window = adaptiveInitialWindow
	}
	l.cond = sync.NewCond(&l.mu)
	return l
}

// Acquire blocks until an in-flight slot is free under the current window,
// then claims it. Returns ctx.Err() if ctx is cancelled while waiting. Pair
// each successful Acquire with exactly one ReleaseSlot.
func (l *UploadLimiter) Acquire(ctx context.Context) error {
	// Wake the cond when ctx is cancelled so a waiter parked at a full window
	// does not block forever once the mirror pass is being torn down.
	stop := context.AfterFunc(ctx, func() {
		l.mu.Lock()
		l.cond.Broadcast()
		l.mu.Unlock()
	})
	defer stop()

	l.mu.Lock()
	defer l.mu.Unlock()
	for l.inflight >= l.limitLocked() {
		if err := ctx.Err(); err != nil {
			return err
		}
		l.cond.Wait()
	}
	l.inflight++
	return nil
}

// ReleaseSlot frees a slot claimed by Acquire and wakes one waiter. It does
// NOT feed the controller — call Observe (once, with the upload outcome) for
// that. Always defer ReleaseSlot after a successful Acquire, even on paths
// that never reach the network.
func (l *UploadLimiter) ReleaseSlot() {
	l.mu.Lock()
	if l.inflight > 0 {
		l.inflight--
	}
	l.cond.Signal()
	l.mu.Unlock()
}

// Observe feeds one completed upload into the adaptive controller: rtt is the
// per-PUT latency, bytes the object size, uploadErr its result. No-op in fixed
// mode. Call exactly once per real remote Put — not on slot-only paths (e.g. a
// local-read miss that never reaches the network).
func (l *UploadLimiter) Observe(bytes int, rtt time.Duration, uploadErr error) {
	if !l.adaptive {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	if uploadErr != nil {
		// Rare hard-safety path: multiplicative decrease only.
		l.window = math.Max(1, l.window*errorBackoff)
		return
	}
	if rtt <= 0 || bytes <= 0 {
		return
	}

	// Latency-gradient (Vegas/Gradient2): baseRTT is the propagation floor;
	// rtt above it means the uplink queue is filling. gradient < 1 pulls the
	// window down; the sqrt headroom keeps it gently probing upward.
	if l.baseRTT == 0 || rtt < l.baseRTT {
		l.baseRTT = rtt
	}
	gradient := math.Min(1.0, float64(l.baseRTT)/float64(rtt))
	gradient = math.Max(minGradient, gradient)
	headroom := math.Sqrt(l.window)
	target := l.window*gradient + headroom
	proposed := l.window*(1-windowSmoothing) + target*windowSmoothing

	// Aggregate-goodput gate: integrate delivered bytes over wall time. When
	// goodput stops rising, freeze the growth ceiling at the window that hit
	// peak throughput — the link is saturated even if latency stays flat.
	l.delivered += int64(bytes)
	if !l.sampledOnce {
		l.lastSample = l.now()
		l.sampledOnce = true
	}
	if elapsed := l.now().Sub(l.lastSample); elapsed >= goodputSampleInterval {
		gp := float64(l.delivered) / elapsed.Seconds()
		if l.goodputEWMA == 0 {
			l.goodputEWMA = gp
		} else {
			l.goodputEWMA = l.goodputEWMA*(1-goodputSmoothing) + gp*goodputSmoothing
		}
		// Compare this sample to the previous peak — set BEFORE we overwrite
		// it. If goodput improved, the extra concurrency is still buying
		// throughput: advance the watermark and keep ramping. If it did not,
		// the link has plateaued: freeze growth at the window that achieved
		// peak throughput.
		if l.goodputEWMA > l.peakGoodput*(1+goodputGrowthEpsilon) {
			l.peakGoodput = l.goodputEWMA
			l.windowAtPeak = l.window
			l.plateaued = false
		} else {
			l.plateaued = true
		}
		l.delivered = 0
		l.lastSample = l.now()
	}

	// Once goodput has plateaued, do not grow past the window that achieved
	// peak throughput, even if latency still looks calm (bufferbloat guard).
	// Before any plateau is detected the initial ramp runs unimpeded.
	if l.plateaued && l.windowAtPeak > 0 && proposed > l.windowAtPeak {
		proposed = l.windowAtPeak
	}

	grew := proposed > l.window
	l.window = math.Max(1, math.Min(proposed, float64(l.cap)))
	if grew {
		// Window opened: more than one parked waiter may now fit.
		l.cond.Broadcast()
	}
}

// Limit returns the current integer in-flight ceiling.
func (l *UploadLimiter) Limit() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.limitLocked()
}

// limitLocked returns the window floored and clamped to [1, cap]. Caller holds mu.
func (l *UploadLimiter) limitLocked() int {
	w := int(l.window)
	if w < 1 {
		w = 1
	}
	if w > l.cap {
		w = l.cap
	}
	return w
}
