package engine

import (
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
)

// Bind installs a binding of s over j and h, copying the share table as a
// share's attach does, for a test that cannot bind through Bind and Start.
func (e *Engine) BindForTest(s Share, j Journal, h Handle) {
	t := bindings{}
	if old := e.shares.Load(); old != nil {
		for k, v := range *old {
			t[k] = v
		}
	}
	t[s.ID] = &binding{share: s, j: j, h: h}
	e.shares.Store(&t)
}

// BindingOf reports whether share s is bound, through the content path's
// own lookup.
func (e *Engine) BindingOf(s entity.ShareID) error {
	_, err := e.bindingOf(s)
	return err
}

// FileEntries is the count of files holding an entry.
func (e *Engine) FileEntries() int {
	n := 0
	for i := range e.files.shards {
		sh := &e.files.shards[i]
		sh.mu.Lock()
		n += len(sh.m)
		sh.mu.Unlock()
	}
	return n
}

// HasEntry reports whether f has an entry, through entryOf.
func (e *Engine) HasEntry(f entity.FileID) bool { return e.files.entryOf(f) != nil }

// SameShard reports whether a and b share a shard of the entry map.
func (e *Engine) SameShard(a, b entity.FileID) bool { return e.files.shardOf(a) == e.files.shardOf(b) }

// HoldEntry takes f's entry lock, inserting the entry, and returns the
// function that releases it, as a call adding to it would.
func (e *Engine) HoldEntry(f entity.FileID) func() {
	fe := e.files.lockFor(f)
	return func() { e.files.unlock(f, fe) }
}

// AddPending adds a pending write of [off, off+n) to f's entry, and returns
// the function that drops it again, as a commit would.
func (e *Engine) AddPending(f entity.FileID, off, n int64) (drop func()) {
	fe := e.files.lockFor(f)
	fe.ops = append(fe.ops, pendingOp{kind: opWrite})
	fe.ops[len(fe.ops)-1].extent.Off, fe.ops[len(fe.ops)-1].extent.Len = off, n
	e.files.unlock(f, fe)
	return func() {
		fe := e.files.lockOf(f)
		if fe == nil {
			return
		}
		if len(fe.ops) > 0 {
			fe.ops = fe.ops[1:]
		}
		e.files.unlock(f, fe)
	}
}

// FileStateWaits is the file_state lock-wait series: its count and total.
func (e *Engine) FileStateWaits() (int64, time.Duration) {
	return e.files.waits.count.Load(), time.Duration(e.files.waits.total.Load())
}

// FileStateWaiting is the callers waiting for an entry lock now.
func (e *Engine) FileStateWaiting() int64 { return e.files.waits.waiting.Load() }

// PendingOpType is the pending operation's type, for a check of its fields.
type PendingOpType = pendingOp
