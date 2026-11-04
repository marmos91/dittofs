package segment

import (
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
