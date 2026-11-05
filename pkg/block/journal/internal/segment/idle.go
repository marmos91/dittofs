package segment

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// IdleSealer is the worker that seals early the active segment of any stream
// that has taken no append for core.IdleSealAge while holding more than its
// threshold beyond its last catalogued point (RFC 28 §3.1) — the rule that
// bounds the tail scan every open of an active segment pays. One per journal:
// it walks the journal's own streams on the clock it was handed, so a stream
// idles only by that clock's time, and a stream that never sees one is never
// sealed by it.
type IdleSealer struct {
	streams   *Streams
	clock     core.Clock
	threshold int64
	admit     func() error // the read-only check; nil means writable (R89)
	log       *slog.Logger

	// decision: mu is a leaf lock over the worker's lifecycle flags alone,
	// none of the six domains RFC 1 §8.2 names. It guards started and
	// stopped, is never held across I/O or a stream call, and the stream
	// state the worker reads stays under each stream's own append domain.
	// Withdrawn the day the worker grows state another goroutine shares.
	mu      sync.Mutex // guards started, stopped
	started bool
	stopped bool

	done chan struct{}
	stop sync.Once
	wg   sync.WaitGroup
}

// NewIdleSealer returns the idle sealer over streams, aging them on clock,
// sealing early the active segment of a stream that has taken no append for
// core.IdleSealAge while holding more than threshold beyond its last
// catalogued point. Neither bound is measured; they are constants of the
// implementation (RFC 28 §3.1):
//
// decision: core.IdleSealAge and the threshold are unmeasured constants. They
// are long enough that a stream taking steady writes never seals early, and
// short enough that an idle tail is usually catalogued before a restart. They
// are overturned by active-segment scans dominating time to first read, or by
// a trickle workload whose early seals show up in footer writes.
//
// A nil streams or clock, or a negative threshold, is a caller bug and
// panics. A nil admit means the journal is writable; a nil log discards. The
// constructor installs the clock on every stream, so the idle stamp is live
// from construction: a stream that idled before the sealer existed counts as
// just active from the moment it starts being watched, never as stale. It
// does not start the goroutine — Start does, and Stop joins it.
func NewIdleSealer(streams *Streams, clock core.Clock, threshold int64, admit func() error, log *slog.Logger) *IdleSealer {
	if streams == nil {
		panic("segment: an idle sealer needs its streams")
	}
	if clock == nil {
		panic("segment: an idle sealer needs its clock")
	}
	if threshold < 0 {
		panic(fmt.Sprintf("segment: the idle-seal threshold %d is below 0, so no active segment could ever pass it", threshold))
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	for i := range streams.Len() {
		streams.Stream(i).watchIdle(clock)
	}
	return &IdleSealer{
		streams:   streams,
		clock:     clock,
		threshold: threshold,
		admit:     admit,
		log:       log,
		done:      make(chan struct{}),
	}
}

// Start launches the worker. It is idempotent, and a no-op after Stop: a
// stopped sealer is joined for good, the segments it touched closing after
// it under the caller that stopped it (RFC 1 §8.7).
func (w *IdleSealer) Start() {
	w.mu.Lock()
	if w.started || w.stopped {
		w.mu.Unlock()
		return
	}
	// The worker is registered under mu, so a Stop racing Start cannot set
	// stopped, close done and return from its Wait before the worker it must
	// join exists. The goroutine starts under mu too: no lock of this domain
	// is held across anything but this bookkeeping — the pass it runs takes
	// the streams' own locks alone.
	w.started = true
	w.wg.Add(1)
	go w.run()
	w.mu.Unlock()
}

// Stop stops the worker and joins it: a seal in flight finishes before Stop
// returns (R70 — the caller stops the idle seal before the segments it
// touches are closed), and no pass a Stop raced can still seal, for the
// worker checks done first at each turn's top as well. It returns at once
// before Start, and a second Stop is a no-op.
func (w *IdleSealer) Stop() {
	w.stop.Do(func() {
		w.mu.Lock()
		w.stopped = true
		w.mu.Unlock()
		close(w.done)
	})
	w.wg.Wait()
}

// run is the worker's loop: one pass, then a wait for the pass's own next
// deadline on the clock, until Stop closes done. The done check at the top of
// each turn keeps a worker that was stopped while its previous wait was in
// flight from starting a pass a Stop already waited past.
func (w *IdleSealer) run() {
	defer w.wg.Done()
	for {
		select {
		case <-w.done:
			return
		default:
		}
		wait := w.pass()
		select {
		case <-w.done:
			return
		case <-w.clock.After(wait):
		}
	}
}

// pass walks every stream once and returns how long the worker should wait
// before its next walk. A stream past both bounds — idle for the age, holding
// more than the threshold beyond its last catalogued point — is sealed early
// unless the read-only check refuses it; a stream not due names the wait until
// it is.
//
// ponytail: one worker scans every stream on each wake, O(streams) per wake,
// with at most one wake per IdleSealAge plus one per distinct deadline. The
// upgrade, a deadline heap keyed by stream, is worth it when a profile shows
// the scan.
//
// The fallback wait is core.IdleSealAge, never 0, so a refusing check or a
// failed seal cannot spin the worker on After(0). It is safe as a bound too:
// a stream that is not a candidate now can only become one through an append
// at a time t at or past now, so its deadline t+IdleSealAge is never before
// now+IdleSealAge. Waking then and recomputing arms the exact remaining wait,
// so no seal is ever late.
//
// decision: core.IdleSealAge and the threshold are unmeasured constants, as
// the constructor's decision comment carries in full. What would change them
// here is the same evidence it names: scans of active tails dominating time to
// first read, or a trickle workload whose early seals cost more than they
// return.
func (w *IdleSealer) pass() time.Duration {
	now := w.clock.Elapsed()
	next := core.IdleSealAge

	for i := 0; i < w.streams.Len(); i++ {
		s := w.streams.Stream(i)
		due, wait := s.idleDue(now, w.threshold)
		if !due {
			if wait > 0 {
				next = min(next, wait)
			}
			continue
		}

		// decision: R89 — while the journal is read-only the idle seal
		// writes nothing it need not, so it stops while the check fails. The
		// cost is that an idle tail stays unsealed and is scanned at the next
		// open. Withdrawn if the read-only state ever allows writes that a
		// seal needs. The check runs outside any stream lock, holding nothing
		// the writer of the read-only flag could wait on; a stream that came
		// due meanwhile is re-read by the seal's own held check.
		if w.admit != nil {
			if err := w.admit(); err != nil {
				w.log.Debug("segment: the idle seal was refused by the read-only check",
					"stream", i, "err", err)
				continue
			}
		}

		sealed, res, err := s.sealIdle(now, w.threshold)
		switch {
		case !sealed && err == nil:
			// The re-check under the seal's own hold refused: an append
			// landed, another seal ran, the stream closed. The stream is
			// exactly as its own holder left it — nothing to report.
		case err != nil && (errors.Is(err, ErrStreamClosed) || errors.Is(err, ErrWrongState)):
			w.log.Debug("segment: the idle seal lost the segment to a close or an explicit seal",
				"stream", i, "err", err)
		case err != nil:
			// The segment is ended: its next Sync reports ErrSegmentEnded to
			// the durability layer, which owns the records it acknowledged.
			// The worker retries no segment and never rolls.
			w.log.Error("segment: the idle seal failed and its segment is ended",
				"stream", i, "err", err)
		case res.TailErr != nil:
			w.log.Warn("segment: the idle seal left the tail; the segment is sealed at its cap",
				"stream", i, "err", res.TailErr)
		case sealed:
			w.log.Debug("segment: the idle seal returned the segment's tail",
				"stream", i, "marker", res.Marker, "size", res.Size)
		}
	}
	return next
}
