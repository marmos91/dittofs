package smb

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
)

// DefaultOrderWaitTimeout bounds how long a response waits for the responses
// ahead of it. Nothing is expected to reach it: every handler wait that depends
// on further client traffic releases its slot before blocking. It exists so a
// handler that blocks unexpectedly delays one connection's responses instead of
// stalling them forever, and it logs when it fires.
const DefaultOrderWaitTimeout = 30 * time.Second

// RequestOrder restores, per connection, an ordering a single-threaded server
// gets for free: a response never reaches the wire ahead of a lease or oplock
// break notification that an earlier request on the same connection owes.
//
// Break notifications are written synchronously by the handler that triggers
// them, before that handler's own response. So ordering response emission by
// arrival is enough to order breaks against later responses too.
//
// Only the read loop knows arrival order — it reads sequentially, while the
// handler goroutines it spawns are scheduled in any order. It therefore issues
// one token per request, in wire order, before spawning. Handlers keep running
// concurrently; only the moment a response is written is ordered.
//
// A handler that must wait for something only a *later* request can deliver —
// a lease break acknowledgement, say — releases its token before blocking.
// Without that, the client would be waiting for a response the server is
// holding behind the very request the client cannot send yet.
type RequestOrder struct {
	mu sync.Mutex
	// wake is closed and replaced on every release, waking all waiters to
	// re-check. Waiters are one per in-flight request on one connection.
	wake     chan struct{}
	next     uint64
	low      uint64 // every sequence below this has been released
	released map[uint64]struct{}

	waitTimeout time.Duration
}

// NewRequestOrder creates the ordering state for one connection.
func NewRequestOrder() *RequestOrder {
	return &RequestOrder{
		wake:        make(chan struct{}),
		released:    make(map[uint64]struct{}),
		waitTimeout: DefaultOrderWaitTimeout,
	}
}

// OrderToken is one request's place in its connection's response order.
// A nil token is inert, so paths with no ordering (tests, direct dispatch)
// need no special casing.
type OrderToken struct {
	order    *RequestOrder
	seq      uint64
	released atomic.Bool
}

// Begin claims the next place in the order. Call it from the read loop, in
// wire order, before the handler goroutine is spawned.
func (o *RequestOrder) Begin() *OrderToken {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	seq := o.next
	o.next++
	return &OrderToken{order: o, seq: seq}
}

// WaitTurn blocks until every request that arrived before this one has
// released its token. Call it immediately before writing a response.
//
// It returns early — leaving this response free to overtake — when ctx is
// cancelled or the wait timeout expires, since a stalled connection is worse
// than an ordering violation on a connection that is already in trouble.
func (t *OrderToken) WaitTurn(ctx context.Context) {
	if t == nil {
		return
	}
	o := t.order
	deadline := time.NewTimer(o.waitTimeout)
	defer deadline.Stop()

	for {
		o.mu.Lock()
		if o.low >= t.seq {
			o.mu.Unlock()
			return
		}
		wake := o.wake
		o.mu.Unlock()

		select {
		case <-wake:
		case <-ctx.Done():
			return
		case <-deadline.C:
			logger.Warn("SMB response ordering timed out; response may overtake an earlier request's break notification",
				"sequence", t.seq,
				"timeout", o.waitTimeout)
			return
		}
	}
}

// Release gives up this request's place, letting the responses behind it go
// out. It is idempotent: the read loop defers it for every request, and a
// handler may call it earlier when it is about to block on client traffic.
func (t *OrderToken) Release() {
	if t == nil || !t.released.CompareAndSwap(false, true) {
		return
	}
	o := t.order
	o.mu.Lock()
	o.released[t.seq] = struct{}{}
	for {
		if _, ok := o.released[o.low]; !ok {
			break
		}
		delete(o.released, o.low)
		o.low++
	}
	close(o.wake)
	o.wake = make(chan struct{})
	o.mu.Unlock()
}

// orderTokenKey types the context value below.
type orderTokenKey struct{}

// WithOrderToken attaches a request's ordering token to its context. The token
// travels with the request rather than through every dispatch signature it
// crosses, the same way the request's deadline does.
func WithOrderToken(ctx context.Context, t *OrderToken) context.Context {
	if t == nil {
		return ctx
	}
	return context.WithValue(ctx, orderTokenKey{}, t)
}

// OrderTokenFrom returns the request's ordering token, or nil when the caller
// dispatched without one.
func OrderTokenFrom(ctx context.Context) *OrderToken {
	t, _ := ctx.Value(orderTokenKey{}).(*OrderToken)
	return t
}
