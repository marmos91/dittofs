package segment

import (
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// WantRecycleForTest runs wantRecycle: the retire tests reach the pool's
// recycle reservation from the external test package, which cannot name the
// unexported method.
func (p *Pool) WantRecycleForTest() (Class, bool) { return p.wantRecycle() }

// RecycledForTest runs recycled with the reservation wantRecycle took.
func (p *Pool) RecycledForTest(id core.SegmentID, c Class) { p.recycled(id, c) }

// RecycleFailedForTest runs recycleFailed on the class the reservation was
// taken for.
func (p *Pool) RecycleFailedForTest(c Class) { p.recycleFailed(c) }

// RetryEveryForTest shortens the retirer's retry interval, so a test's
// worker pass arrives in milliseconds rather than the second the default
// asks. It must run before Start: the ticker it feeds is the worker's own.
func RetryEveryForTest(r *Retirer, d time.Duration) { r.retryEvery = d }

// KickForTest wakes the retirer's worker from the external test package.
func (r *Retirer) KickForTest() { r.kickWorker() }

// PendingForTest reads the demand level a refused Hold recorded, so a check
// can assert what a made or recycled spare paid down.
func (p *Pool) PendingForTest() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pending
}

// StoppedForTest reports whether Stop has closed the worker's done, so a
// check can sequence a Stop that must land while the worker is mid-flight:
// the closed channel is the stop flag, observable without racing the join
// the Stop itself blocks in.
func (r *Retirer) StoppedForTest() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

// ExtraNamesForTest reports how many extra names the executor still records
// for a segment id, so a check can assert a completed retirement leaves none
// behind: the map is bounded by the retirements still owed a retry.
func (r *Retirer) ExtraNamesForTest(id core.SegmentID) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.extra[id])
}
