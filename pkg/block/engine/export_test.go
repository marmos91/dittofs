package engine

import (
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// ErrNotStarted is the answer of a Bind while no journal is open.
var ErrNotStarted = errNotStarted

// The errors start answers and refuses shares with, for a test to match.
var (
	ErrUnbuilt      = errUnbuilt
	ErrStarted      = errStarted
	ErrJournalLost  = errJournalLost
	ErrNotRecorded  = errNotRecorded
	ErrUnidentified = errUnidentified
)

// SetJournalsForTest sets the journals the engine has open, as Start will.
func (e *Engine) SetJournalsForTest(js ...Journal) {
	e.mu.Lock()
	e.journals = js
	e.mu.Unlock()
}

// MarkRaisedForTest records shard as raised by this process, as Start will.
func (e *Engine) MarkRaisedForTest(shard entity.ShardID) {
	e.mu.Lock()
	e.raised[shard] = struct{}{}
	e.mu.Unlock()
}

// TableForTest is the share table's pointer, compared for identity.
func (e *Engine) TableForTest() any {
	return e.shares.Load()
}

// StateOf is share s's binding state, zero when it has no binding.
func (e *Engine) StateOf(s entity.ShareID) ShareState {
	b := e.table()[s]

	if b == nil {
		return 0
	}

	return ShareState(b.state.Load())
}

// LifecycleFree reports whether the lifecycle mutex is free now.
func (e *Engine) LifecycleFree() bool {
	if !e.mu.TryLock() {
		return false
	}

	e.mu.Unlock()

	return true
}

// HoldLifecycle takes the lifecycle mutex and returns the function that
// releases it.
func (e *Engine) HoldLifecycle() (release func()) {
	e.mu.Lock()

	return e.mu.Unlock
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
// the function that drops it again, as a commit would, bumping the removal
// counter.
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
			e.files.removals.Add(1)
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

// SetSample replaces how a write samples its verifier.
func (e *Engine) SetSample(f func() vfserr.Verifier) { e.sample = f }

// PendingView is one pending operation as a test reads it.
type PendingView struct {
	Version   journal.Version
	Off, Len  int64
	At        time.Time
	SetsMtime bool
	HasDone   bool
}

// Pending is f's pending operations, in the entry's order.
func (e *Engine) Pending(f entity.FileID) []PendingView {
	fe := e.files.lockOf(f)
	if fe == nil {
		return nil
	}
	defer e.files.unlock(f, fe)
	var out []PendingView
	for _, op := range fe.ops {
		out = append(out, PendingView{op.version, op.extent.Off, op.extent.Len, op.at, op.setsMtime, op.done != nil})
	}
	return out
}

// AddOp adds a pending deallocate of [off, off+n), or with truncate a pending
// truncate to off, to f's entry at version v, as those calls will once built.
func (e *Engine) AddOp(f entity.FileID, v uint64, truncate bool, off, n int64) {
	op := pendingOp{version: journal.Version{Counter: v}, kind: opDeallocate, extent: journal.Extent{Off: off, Len: n}}
	if truncate {
		op.kind, op.extent.Len = opTruncate, 0
	}
	fe := e.files.lockFor(f)
	fe.insert(op)
	e.files.unlock(f, fe)
}

// AddWriteOp adds a pending write of [off, off+n) to f's entry at version v.
func (e *Engine) AddWriteOp(f entity.FileID, v uint64, off, n int64) {
	fe := e.files.lockFor(f)
	fe.insert(pendingOp{version: journal.Version{Counter: v}, kind: opWrite, extent: journal.Extent{Off: off, Len: n}})
	e.files.unlock(f, fe)
}

// ClearOps drops f's pending operations, as a commit of their existence does.
func (e *Engine) ClearOps(f entity.FileID) {
	fe := e.files.lockFor(f)
	fe.ops = nil
	e.files.removals.Add(1)
	e.files.unlock(f, fe)
}

// Reads is engine_reads_total of class, by its label.
func (e *Engine) Reads(class string) int64 {
	for c, name := range readClassNames {
		if name == class {
			return e.reads.byClass[c].Load()
		}
	}
	panic("no read class " + class)
}

// ReadSeconds is engine_read_seconds: its count and total.
func (e *Engine) ReadSeconds() (int64, time.Duration) {
	return e.reads.count.Load(), time.Duration(e.reads.total.Load())
}
