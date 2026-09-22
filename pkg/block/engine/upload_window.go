package engine

import (
	"context"
	"time"
)

// InFlightUploads reports how many packed blocks are currently in flight to the
// remote: one per upload-window slot held, taken at submit and released once
// that block's CommitBlock returns. It is the live counterpart to the lifetime
// CompletedSyncs/FailedSyncs, and the only queue-depth number the carve path
// has — the dispatcher enumerates files per tick rather than keeping a queue,
// so there is nothing "waiting" to count behind these.
//
// decision: the slot spans the block's metadata commit as well as its PutBlock,
// so this counts uploads in progress rather than bytes strictly on the wire,
// and a slow commit holds a slot after the upload finished. That is the honest
// reading for an operator asking "is the remote still being written to", and it
// is the same occupancy that refuses new uploads. The controller deliberately
// does NOT sample this for window sizing — it counts PutBlock concurrency
// directly (takePutPeak), because for THAT question the commit tail is a
// misread. Revisit here only if arenas ever get a lifetime independent of the
// slot, which would let the two numbers diverge in a way an operator cares
// about.
//
// Nil-safe: NewRemoteSync always builds the limiter, but RemoteSync is also
// assembled as a bare struct literal in tests, and a stats read must not panic.
func (m *RemoteSync) InFlightUploads() int {
	if m.uploadLimiter == nil {
		return 0
	}
	return m.uploadLimiter.InFlight()
}

// noteBlockUploaded feeds the goodput sample with one block's bytes as soon as
// its PutBlock returns. It is deliberately not the same moment as
// noteBlockCommitted: the controller resizes the upload window, so its sample
// has to be the uplink alone and not the per-file-serialized metadata commit
// that follows.
//
// decision: a block whose PutBlock succeeds and whose metadata commit then
// fails still counts its bytes here, with no compensating error flag on the
// Flush/SyncNow/Drain paths (only carvePass feeds uploadErrWindow). That is
// intended rather than overlooked: those bytes did cross the uplink, and a
// commit failure is not a signal to back the upload window off — backing off
// would answer a metadata fault by throttling a healthy link. The caller still
// gets the error. Revisit if commit failures ever correlate with uplink faults,
// where suppressing the bytes would become the honest reading.
func (m *RemoteSync) noteBlockUploaded(bytes int64) {
	m.uploadedBytesWindow.Add(bytes)
}

// putInFlightBits is the width of the live-count half of putSample; the peak
// occupies the other half. Upload concurrency is bounded by the window
// (MaxParallelUploads at the very most), so neither half can approach 2^32.
const putInFlightBits = 32

func packPutSample(peak, inFlight uint32) uint64 {
	return uint64(peak)<<putInFlightBits | uint64(inFlight)
}

func unpackPutSample(v uint64) (peak, inFlight uint32) {
	return uint32(v >> putInFlightBits), uint32(v)
}

// notePutInFlight brackets one PutBlock: +1 before the call, -1 after it
// returns (success or failure). Delta rather than a start/end pair keeps it to
// one sink hook.
//
// The count and the peak move together in one compare-and-swap, so a sampler
// never sees a PUT counted in one and missing from the other. This is a CAS
// loop rather than a mutex on purpose: it brackets every upload, and the
// previous version already ran a CAS loop here to raise the peak, so nothing
// on the hot path got slower.
func (m *RemoteSync) notePutInFlight(delta int64) {
	if delta == 0 {
		return
	}
	for {
		old := m.putSample.Load()
		peak, inFlight := unpackPutSample(old)
		if delta > 0 {
			inFlight++
			if inFlight > peak {
				peak = inFlight
			}
		} else {
			if inFlight == 0 {
				return // unbalanced release; refuse to wrap the counter
			}
			inFlight--
		}
		if m.putSample.CompareAndSwap(old, packPutSample(peak, inFlight)) {
			return
		}
	}
}

// takePutPeak returns the high-water mark of concurrent PutBlock calls since
// the last call and resets it to the count still in flight — the same contract
// as DynamicSemaphore.TakePeak, so a control interval never inherits a peak
// that belongs to an earlier one. Read and reset are one compare-and-swap, so
// no upload can slip between them.
func (m *RemoteSync) takePutPeak() int {
	for {
		old := m.putSample.Load()
		peak, inFlight := unpackPutSample(old)
		if m.putSample.CompareAndSwap(old, packPutSample(inFlight, inFlight)) {
			return int(peak)
		}
	}
}

// runUploadController is the adaptive upload-concurrency control loop.
// Every interval it turns the bytes/error accumulated by carveAndCommitBlock
// into a goodput sample, feeds the GoodputController, and applies the returned
// window to the shared uploadLimiter. Runs only in adaptive mode (controller
// non-nil). Idle intervals (no bytes, nothing in flight, no error) are skipped
// so a write pause is not misread as a goodput collapse.
func (m *RemoteSync) runUploadController(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	// Publish the starting window so the gauge is populated before the first
	// adjustment.
	if mx := m.dataplaneMetrics(); mx != nil {
		mx.SetUploadWindow(m.uploadLimiter.Limit())
	}
	seconds := interval.Seconds()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.adaptiveUploadTick(seconds)
		}
	}
}

// adaptiveUploadTick converts one control interval's accumulated bytes and
// error flag into a goodput sample, feeds the controller, and applies the
// resulting window to the upload limiter. Extracted from the goroutine loop so
// the bytes→goodput→window glue is unit-testable without a clock. intervalSec
// is the control interval in seconds.
func (m *RemoteSync) adaptiveUploadTick(intervalSec float64) {
	bytes := m.uploadedBytesWindow.Swap(0)
	sawErr := m.uploadErrWindow.Swap(0) > 0
	// Peak in-flight over the interval distinguishes window-limited from
	// app-limited: uploads that filled the window mean goodput reflects the
	// window; otherwise the upstream carve pipeline was the constraint (see
	// syncer.GoodputController.Observe).
	//
	// Sampled from PutBlock concurrency rather than from the semaphore, whose
	// slots also span the metadata commit. Reading the semaphore here let a
	// slow commit hold slots after the uploads were done and report a full
	// window, so the controller settled on a metadata bottleneck believing it
	// had found the uplink knee.
	peak := m.takePutPeak()
	windowLimited := peak >= m.uploadLimiter.Limit()

	if bytes == 0 && peak == 0 && !sawErr {
		// Idle interval: no control decision, but publish an honest zero so the
		// goodput gauge does not freeze at the last active sample.
		if mx := m.dataplaneMetrics(); mx != nil {
			mx.SetUploadGoodput(0)
		}
		return
	}

	goodput := float64(bytes) / intervalSec
	window := m.uploadController.Observe(goodput, windowLimited, sawErr)
	m.uploadLimiter.SetLimit(window)
	if mx := m.dataplaneMetrics(); mx != nil {
		mx.SetUploadWindow(window)
		mx.SetUploadGoodput(goodput)
	}
}
