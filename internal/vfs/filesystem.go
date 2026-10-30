// Package vfs is the filesystem: the one component that orders every
// call across the metadata store, open state and the engine. *Filesystem
// implements vfs.Service. Only the node holds the *Filesystem: it builds it,
// starts it and stops it, and hands adapters the Service alone.
package vfs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/marmos91/dittofs/internal/vfs/lockorder"
	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
	"github.com/marmos91/dittofs/pkg/openstate"
	"github.com/marmos91/dittofs/pkg/vfs"
)

// Deps is what the filesystem is built over. The node starts and stops each of
// them; the filesystem closes none.
//
// ponytail: Metrics is a prometheus.Registerer, which takes any label set.
// Replace it with RFC 25's Registry once it exists: that refuses a per-share
// label on a counter, the cardinality a node serving many shares cannot carry.
type Deps struct {
	Meta    *kvstore.Store
	Open    *openstate.Table // the node starts it with the filesystem as its Sink and Admission as its Order
	Engine  Content
	Events  vfs.Events            // never blocks, and counts what it drops; nil: the null sink
	Metrics prometheus.Registerer // nil: the filesystem's metrics are kept but not registered
}

// Content is the engine's content methods, declared where the filesystem calls
// them. The engine satisfies it, and a fake does in unit tests.
//
// ponytail: WriteOpts and Overlay are declared here, beside Content, as the
// engine that should declare them is not built. Alias the engine's once it
// declares them, so it satisfies Content unchanged.
type Content interface {
	// Draw is a version for a change stored outside the journal, drawn under
	// the file's accept lock.
	Draw(ctx context.Context, f entity.FileID) (entity.JournalVersion, error)
	Write(ctx context.Context, f entity.FileID, off int64, p []byte, o WriteOpts) (vfs.Verifier, error)
	// Commit makes the range stable, n 0 the whole file, and answers the
	// file's loss sequence.
	Commit(ctx context.Context, f entity.FileID, off, n int64) (v vfs.Verifier, lossSeq uint64, err error)
	// LossSeq is the file's loss sequence now, with no sync.
	LossSeq(f entity.FileID) uint64
	// Read answers the verified bytes read, then the error.
	Read(ctx context.Context, f entity.FileID, off int64, dst []byte) (int, error)
	Truncate(ctx context.Context, f entity.FileID, size int64) error
	Deallocate(ctx context.Context, f entity.FileID, off, n int64) error
	// Delete is the file's release; never the journal's eviction.
	Delete(ctx context.Context, f entity.FileID) error
	// Clone is atomic; Copy copies xs in order and may be short.
	Clone(ctx context.Context, src, dst entity.FileID, x vfs.Extent) error
	Copy(ctx context.Context, src, dst entity.FileID, xs []vfs.Extent) (int64, error)
	// Overlay is the file's size, times and version as the journal's
	// uncommitted operations leave them, replayed in version order; pending
	// false, none is, and the File record answers alone.
	Overlay(ctx context.Context, f entity.FileID) (o Overlay, pending bool, err error)
	Allocation(ctx context.Context, f entity.FileID, off int64) (entity.Span, error)
}

// WriteOpts is what a write asks of the engine beside its bytes.
type WriteOpts struct {
	Suspend entity.TimeMask // the times this write leaves alone
	Done    func()          // called exactly once per accepted write: by the commit recording its usage or its loss, or by the engine, charging nothing, when it drops the write uncommitted; never if Write fails
}

// Overlay is a file's size, times and version as the journal's uncommitted
// operations leave them.
type Overlay struct {
	Size         int64 // SizeSet: the size; else the end of the pending writes
	SizeSet      bool  // a truncate is pending, so Size is exact
	Mtime, Ctime time.Time
	Version      entity.JournalVersion
}

// state is where the filesystem is in its life: built, serving, draining and
// stopped, once each and in that order.
type state uint8

const (
	built state = iota
	serving
	draining
	stopped
)

var states = [...]string{built: "built", serving: "serving", draining: "draining", stopped: "stopped"}

func (s state) String() string { return states[s] }

// errUnbuilt is the answer of every call the filesystem does not build yet.
//
// decision: an unbuilt call answers ErrUnavailable, which an adapter answers
// as it answers a draining filesystem, rather than a new sentinel no adapter
// maps. It is wrapped, so a test tells it from a drain's refusal. It holds
// while no adapter calls the filesystem; every call must be built, and this
// removed, before one does.
var errUnbuilt = fmt.Errorf("vfs: call not built yet: %w", vfs.ErrUnavailable)

// Filesystem is the filesystem service. It hands each event straight to the
// sink, keeping no queue of its own, so an event is dropped, and counted, in
// one place only: the sink.
type Filesystem struct {
	events vfs.Events
	locks  lockorder.Locks

	mu     sync.Mutex
	state  state
	calls  sync.WaitGroup  // the admitted calls not yet answered
	active int             // how many of calls there are, so Drain tells none from some
	stop   context.Context // ends at Drain's deadline, ending every admitted call's context
	cancel context.CancelFunc

	inflight prometheus.Gauge
}

var (
	_ vfs.Service     = (*Filesystem)(nil)
	_ openstate.Sink  = (*Filesystem)(nil)
	_ openstate.Order = (*Filesystem)(nil).Admission
)

// New builds the filesystem. It starts no goroutine and opens nothing.
func New(d Deps) (*Filesystem, error) {
	if d.Meta == nil || d.Open == nil || d.Engine == nil {
		return nil, errors.New("vfs: a filesystem needs a metadata store, open state and an engine")
	}
	fs := &Filesystem{
		events: d.Events,
		inflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "dittofs", Name: "vfs_inflight_calls",
			Help: "Calls admitted and not yet answered: what a drain waits for.",
		}),
	}
	if fs.events == nil {
		fs.events = noEvents{}
	}
	if d.Metrics != nil {
		if err := d.Metrics.Register(fs.inflight); err != nil {
			return nil, fmt.Errorf("vfs: %w", err)
		}
	}
	fs.stop, fs.cancel = context.WithCancel(context.Background())
	return fs, nil
}

// Start starts serving: from it on, calls are admitted. It is called once,
// on a built filesystem, before any adapter serves.
func (fs *Filesystem) Start(ctx context.Context) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.state != built {
		return fmt.Errorf("vfs: Start of a %s filesystem", fs.state)
	}
	fs.state = serving
	return nil
}

// Drain refuses every new call with ErrUnavailable and returns once every
// admitted call has answered. At ctx's end it ends the admitted calls'
// contexts, still waits for them to answer, and answers ctx's error; with
// no call admitted it answers nil at once, whatever ctx's state. It
// revokes nothing and forces no recall: open state is left to the next
// start's grace.
//
// decision: Drain waits past ctx's end for the calls whose contexts it ended,
// since a call still running once the node stops open state, the engine and
// the store would act on what is stopped. A call that ignores its context
// therefore holds Drain; bound it here if one is found.
func (fs *Filesystem) Drain(ctx context.Context) error {
	fs.mu.Lock()
	if fs.state != serving {
		defer fs.mu.Unlock()
		return fmt.Errorf("vfs: Drain of a %s filesystem", fs.state)
	}
	fs.state = draining
	idle := fs.active == 0
	fs.mu.Unlock()
	if idle {
		return nil
	}

	joined := make(chan struct{})
	go func() {
		fs.calls.Wait()
		close(joined)
	}()
	select {
	case <-joined:
		return nil
	case <-ctx.Done():
		fs.cancel()
		<-joined
		return ctx.Err()
	}
}

// Stop ends the filesystem: it is called once, on a drained filesystem, or on
// a built one whose Start was never called or failed. With no queue and no
// goroutine of its own, it has nothing to flush or join. It closes nothing it
// was handed: the node stops open state, then the engine, the event sink and
// the store, after it.
//
// decision: the lifecycle's close is named Stop, since Close is the Data
// area's close of an open, and *Filesystem carries both.
func (fs *Filesystem) Stop() error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.state != built && fs.state != draining {
		return fmt.Errorf("vfs: Stop of a %s filesystem", fs.state)
	}
	if fs.active > 0 {
		return fmt.Errorf("vfs: Stop with %d calls admitted: Drain has not returned", fs.active)
	}
	fs.state = stopped
	fs.cancel()
	return nil
}

// enter admits one call: ErrUnavailable unless the filesystem is serving. The
// context it answers ends with the caller's or at Drain's deadline, and done
// is called once the call has answered.
//
// ponytail: each call allocates its own cancellable context and a stop
// registration. Cancel admitted calls through a shared context instead, once
// the data path's allocations show in a profile.
func (fs *Filesystem) enter(ctx context.Context) (_ context.Context, done func(), err error) {
	fs.mu.Lock()
	if fs.state != serving {
		fs.mu.Unlock()
		return ctx, nil, vfs.ErrUnavailable
	}
	fs.calls.Add(1)
	fs.active++
	fs.mu.Unlock()
	fs.inflight.Inc()
	ctx, cancel := context.WithCancel(ctx)
	unhook := context.AfterFunc(fs.stop, cancel)
	return ctx, func() {
		unhook()
		cancel()
		fs.inflight.Dec()
		fs.mu.Lock()
		fs.active--
		fs.mu.Unlock()
		fs.calls.Done()
	}, nil
}

// unbuilt is the body of every call the filesystem does not build yet:
// refused while the filesystem is not serving, as every call is, and
// otherwise answered errUnbuilt.
func (fs *Filesystem) unbuilt(ctx context.Context) error {
	_, done, err := fs.enter(ctx)
	if err != nil {
		return err
	}
	done()
	return errUnbuilt
}

// Admission takes file f's admission lock and returns its unlock: open
// state's Order, which the node hands the table at its Start.
func (fs *Filesystem) Admission(f entity.FileID) (unlock func()) {
	return fs.locks.Admission(lockorder.Key(f))
}

// noEvents is the null sink, the one the filesystem hands events to when the
// node supplies none.
type noEvents struct{}

func (noEvents) Quota(context.Context, vfs.QuotaEvent)   {}
func (noEvents) Access(context.Context, vfs.AccessEvent) {}

// unbuiltEach is unbuilt for a call answering one error per reference.
func (fs *Filesystem) unbuiltEach(ctx context.Context, n int) []error {
	err := fs.unbuilt(ctx)
	errs := make([]error, n)
	for i := range errs {
		errs[i] = err
	}
	return errs
}
