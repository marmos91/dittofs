package smb

import "context"

// authSweeper runs authorization re-check sweeps on a goroutine of its own and
// coalesces the requests that pile up while one is running.
//
// The control-plane fires its auth-cache-invalidate subscribers synchronously,
// and the SMB subscriber's work is a full sweep over every session and every
// tree with a store lookup per session. Run inline, that is an operator's API
// call blocking until every SMB connection has been re-resolved.
//
// Coalescing is the other half of the fix rather than a refinement of it. A
// bulk grant edit raises one invalidation per row, so a queue of N sweeps over
// the same two tables is the same stall wearing a different hat. A sweep reads
// current state rather than a delta, so a single run that starts after the last
// request answers every request that arrived before it.
type authSweeper struct {
	// wake carries pending-sweep requests. Capacity one: a request that finds
	// it already full is absorbed by the pending wake, which is the coalescing.
	wake   chan struct{}
	cancel context.CancelFunc
	// done closes when the goroutine has returned, so stop can join it.
	done chan struct{}
}

// newAuthSweeper starts the sweep goroutine. sweep is handed a context that is
// cancelled by stop, so a sweep in flight during shutdown unwinds instead of
// reaching into handler state that is being torn down.
func newAuthSweeper(sweep func(context.Context)) *authSweeper {
	ctx, cancel := context.WithCancel(context.Background())
	s := &authSweeper{
		wake:   make(chan struct{}, 1),
		cancel: cancel,
		done:   make(chan struct{}),
	}

	go func() {
		defer close(s.done)
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
				// A select whose cases are both ready picks between them at
				// random, so a wake delivered as shutdown lands would otherwise
				// start a sweep the cancellation was meant to prevent.
				if ctx.Err() != nil {
					return
				}
				sweep(ctx)
			}
		}
	}()

	return s
}

// request asks for a sweep and returns immediately. It is called from the
// control-plane goroutine, which is the whole point: it must not block there,
// and it never does — the send is non-blocking and a full channel means a sweep
// is already pending.
func (s *authSweeper) request() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// stop cancels any sweep in flight and waits for the goroutine to return, so no
// sweep can touch session or handler state after it. Idempotent — a
// context.CancelFunc does nothing after its first call — and safe to call
// concurrently with request: wake is never closed.
//
// ctx bounds the wait. A sweep does plenty that no context reaches — the
// revalidate mutex, closing a revoked session's opens, draining its parked
// locks — so an unbounded join would let one slow store call hold shutdown
// open, and the caller's shutdown deadline does not cover a Stop that never
// returns. It reports whether the goroutine was actually joined; false means
// the sweep is still running and the caller has to treat handler state as
// still in use. A nil ctx waits without a bound.
func (s *authSweeper) stop(ctx context.Context) bool {
	s.cancel()
	if ctx == nil {
		<-s.done
		return true
	}
	select {
	case <-s.done:
		return true
	case <-ctx.Done():
		return false
	}
}
