package engine

import (
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

func op(v uint64, k opKind, off, n int64) pendingOp {
	return pendingOp{version: journal.Version{Counter: v}, kind: k, extent: journal.Extent{Off: off, Len: n}, at: time.Unix(int64(v), 0), setsMtime: k != opDeallocate}
}

// The replay applies a truncate and the writes after it in version order: a
// truncate sets the size exactly, a later write raises it to its end, and a
// later write below it leaves it, its version the newest. A replay taking the
// maximum of every end fails the truncate's step; one ignoring order fails
// the last.
func TestReplayAppliesOperationsInVersionOrder(t *testing.T) {
	const kib = 1 << 10
	steps := []struct {
		op      pendingOp
		size    int64
		sizeSet bool
	}{
		{op(3, opWrite, 0, 8*kib), 8 * kib, false},
		{op(5, opTruncate, 4*kib, 0), 4 * kib, true},
		{op(6, opWrite, 8*kib, 4*kib), 12 * kib, true},
		{op(7, opWrite, 0, 2*kib), 12 * kib, true},
	}
	var fe fileEntry
	for _, s := range steps {
		fe.insert(s.op)
		o, pending := replay(fe.ops)
		want := entity.JournalVersion(s.op.version.Bytes())
		if !pending || o.Size != s.size || o.SizeSet != s.sizeSet || o.Version != want {
			t.Errorf("after v%d: size %d set %v version %v; want %d %v %v", s.op.version.Counter, o.Size, o.SizeSet, o.Version, s.size, s.sizeSet, want)
		}
	}
	var shuffled fileEntry
	for _, i := range []int{3, 1, 0, 2} {
		shuffled.insert(steps[i].op)
	}
	a, _ := replay(fe.ops)
	b, _ := replay(shuffled.ops)
	if a != b {
		t.Errorf("inserted out of order: %+v, want %+v", b, a)
	}
}

// The times are the newest operation's for Ctime and the newest of one that
// sets it for Mtime: a suspended write moves Ctime alone.
func TestReplayTimes(t *testing.T) {
	w := op(1, opWrite, 0, 1)
	suspended := op(2, opWrite, 1, 1)
	suspended.setsMtime = false
	o, _ := replay([]pendingOp{w, suspended})
	if !o.Mtime.Equal(w.at) || !o.Ctime.Equal(suspended.at) {
		t.Errorf("mtime %v ctime %v; want %v and %v", o.Mtime, o.Ctime, w.at, suspended.at)
	}
}
