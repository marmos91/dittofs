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
