// Package vfs is the filesystem: the one component that orders every
// call across the metadata store, open state and the engine. *Filesystem
// implements vfs.Service. Only the node holds the *Filesystem: it builds it,
// starts it and stops it, and hands adapters the Service alone.
package vfs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/cubbit/dittofs/internal/vfs/authz"
	"github.com/cubbit/dittofs/internal/vfs/lockorder"
	"github.com/cubbit/dittofs/internal/vfs/pseudofs"
	"github.com/cubbit/dittofs/internal/vfs/quota"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/openstate"
	"github.com/cubbit/dittofs/pkg/vfs"
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
	Events  vfs.Events                     // never blocks, and counts what it drops; nil: the null sink
	Audit   func(vfs.ShareID) authz.Policy // each share's audit policy; nil: every share's is off
	Metrics prometheus.Registerer          // nil: the filesystem's metrics are kept but not registered
	Logger  *slog.Logger                   // nil: slog.Default()
}

// Content is the engine's content methods, declared where the filesystem calls
// them. The engine satisfies it, and a fake does in unit tests. Every call
// names the file's share, which the filesystem has from the handle; a share
// with no binding answers a Delay with DelayFrozen, its binding still to come.
// A share being removed never reaches it unbound, as admission answers
// ErrStale from the removal mark.
//
// ponytail: WriteOpts and Overlay are declared here, beside Content, as the
// engine that should declare them is not built, and so is CommitExistence,
// the existence commit an owner or group change runs first, which the
// engine's surface names no method for. Alias the engine's once it
// declares them, so it satisfies Content unchanged.
type Content interface {
	// Draw is a version for a change stored outside the journal, drawn under
	// the file's accept lock.
	Draw(ctx context.Context, s entity.ShareID, f entity.FileID) (entity.JournalVersion, error)
	Write(ctx context.Context, s entity.ShareID, f entity.FileID, off int64, p []byte, o WriteOpts) (vfs.Verifier, error)
	// Commit makes the range stable, n 0 the whole file, and answers the
	// file's loss sequence.
	Commit(ctx context.Context, s entity.ShareID, f entity.FileID, off, n int64) (v vfs.Verifier, lossSeq uint64, err error)
	// CommitExistence commits the file's pending existence now, its size,
	// times and charge included, calling each committed write's Done.
	CommitExistence(ctx context.Context, s entity.ShareID, f entity.FileID) error
	// LossSeq is the file's loss sequence now, with no sync; 0 for a share
	// with no binding.
	LossSeq(s entity.ShareID, f entity.FileID) uint64
	// Read answers the verified bytes read, then the error.
	Read(ctx context.Context, s entity.ShareID, f entity.FileID, off int64, dst []byte) (int, error)
	Truncate(ctx context.Context, s entity.ShareID, f entity.FileID, size int64) error
	Deallocate(ctx context.Context, s entity.ShareID, f entity.FileID, off, n int64) error
	// Delete is the file's release; never the journal's eviction. A release
	// that cannot finish within the call answers a retry channel and no
	// error (RFC 8 §8.1): it yields the outcome once, nil when the release
	// committed, vfserr.ErrFileOpen when a holder aborted it, a Delay with
	// DelayClone once the clone it waited behind is done, for the release to
	// be run again, or ErrUnavailable when the engine's Close or Drop ends it
	// or a journal turned read-only undoes its Delete.
	// An error within the call is vfserr.ErrFileOpen for an abort. The
	// engine's send on the channel never blocks: it has capacity 1, as its
	// receiver may stop listening, abandoned at the filesystem's Stop, which
	// precedes the engine's Close (RFC 17 §2.5).
	Delete(ctx context.Context, s entity.ShareID, f entity.FileID) (retry <-chan error, err error)
	// Clone is atomic; Copy copies xs in order and may be short. Each file
	// is named with its own share, as on one node a clone between two
	// shares of one namespace reaches the engine.
	Clone(ctx context.Context, srcShare entity.ShareID, src entity.FileID, dstShare entity.ShareID, dst entity.FileID, x vfs.Extent) error
	Copy(ctx context.Context, srcShare entity.ShareID, src entity.FileID, dstShare entity.ShareID, dst entity.FileID, xs []vfs.Extent) (int64, error)
	// Overlay is the file's size, times and version as the journal's
	// uncommitted operations leave them, replayed in version order; pending
	// false, none is, and the File record answers alone.
	Overlay(ctx context.Context, s entity.ShareID, f entity.FileID) (o Overlay, pending bool, err error)
	Allocation(ctx context.Context, s entity.ShareID, f entity.FileID, off int64) (entity.Span, error)
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
	meta   *kvstore.Store
	open   *openstate.Table
	engine Content
	checks openstate.Checks
	opens  openstate.Opens
	log    *slog.Logger
	authz  authz.Authz
	events vfs.Events
	quota  *quota.Quotas
	locks  lockorder.Locks
	pseudo atomic.Pointer[pseudofs.Tree] // the pseudo-filesystem last built
	build  chan struct{}                 // holds a token while one call rebuilds pseudo

	clients   openstate.Clients
	recovery  openstate.Recovery
	lockState openstate.Locks
	grants    openstate.Grants
	watches   openstate.Watches
	watchers  watchers       // the watches set here, as their changes are delivered
	reg       *registry      // each client's callback paths
	sending   sync.WaitGroup // the callbacks being delivered and the releases being run
	marks     releaser

	mu       sync.Mutex
	state    state
	starting bool                    // Start is running its steps
	served   map[entity.ShareID]bool // the shares whose shard has been served
	calls    sync.WaitGroup          // the admitted calls not yet answered
	active   int                     // how many of calls there are, so Drain tells none from some
	stop     context.Context         // ends at Drain's deadline, ending every admitted call's context
	cancel   context.CancelFunc
	joins    map[entity.ShareID]*join // the calls counted against each share
	// removed is every share whose removal removeShare has started.
	removed map[entity.ShareID]bool

	beforeShareRead func() // run by admission before it reads the share; nil but in tests
	beforeBuild     func() // run by tree before it reads the share list to build; nil but in tests

	inflight       prometheus.Gauge
	callbackErrors *prometheus.CounterVec
	refusals       prometheus.Counter
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
		meta:      d.Meta,
		open:      d.Open,
		clients:   d.Open.Clients(),
		recovery:  d.Open.Recovery(),
		lockState: d.Open.Locks(),
		grants:    d.Open.Grants(),
		watches:   d.Open.Watches(),
		watchers:  watchers{set: map[vfs.WatchID]watcher{}},
		reg:       newRegistry(),
		build:     make(chan struct{}, 1),
		joins:     map[entity.ShareID]*join{},
		removed:   map[entity.ShareID]bool{},
		served:    map[entity.ShareID]bool{},
		events:    d.Events,
		marks:     openMark{d.Open.Opens()},
		inflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "dittofs", Name: "vfs_inflight_calls",
			Help: "Calls admitted and not yet answered: what a drain waits for.",
		}),
		callbackErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "dittofs", Name: "vfs_callback_errors_total",
			Help: "Callbacks that failed, or found no path to the client, by kind.",
		}, []string{"kind"}),
		refusals: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "dittofs", Name: "vfs_deferred_unlink_refusals_total",
			Help: "Deletes on close refused after their client closed: each file kept its name.",
		}),
	}
	if fs.events == nil {
		fs.events = noEvents{}
	}
	fs.engine, fs.checks, fs.opens, fs.log = d.Engine, d.Open.Checks(), d.Open.Opens(), d.Logger
	if fs.log == nil {
		fs.log = slog.Default()
	}
	fs.quota = quota.New(capacity{d.Meta.Capacity()}, fs.events.Quota)
	fs.authz = authz.Authz{Files: d.Meta.Files(), Policy: d.Audit, Emit: fs.events.Access}
	if d.Metrics != nil {
		for _, c := range []prometheus.Collector{fs.inflight, fs.callbackErrors, fs.refusals} {
			if err := d.Metrics.Register(c); err != nil {
				return nil, fmt.Errorf("vfs: %w", err)
			}
		}
	}
	fs.stop, fs.cancel = context.WithCancel(context.Background())
	return fs, nil
}

// Start starts serving: from it on, calls are admitted. It is called once,
// on a built filesystem, before any adapter serves, and after the node has
// started open state with the filesystem as its Sink. It serves the shards
// first, so each installs its persistent opens and enters grace, then ends
// the open state of each share being removed; it does not wait for grace to
// end. A step that fails leaves the filesystem built, and Start answers its
// error.
//
// decision: a shard a failed Start served stays served, where RFC 17 §2.5
// stops what the earlier steps started, as open state builds no
// ReleaseShard (RFC 14 §11.3) to unserve it: a retried Start serves only the
// shards not yet served, and a node giving up stops open state. Meanwhile a
// served shard's grace end still runs its pending releases from the built
// filesystem. Unserve them here once ReleaseShard is built.
func (fs *Filesystem) Start(ctx context.Context) error {
	fs.mu.Lock()
	if fs.state != built || fs.starting {
		defer fs.mu.Unlock()
		return fmt.Errorf("vfs: Start of a %s filesystem", fs.state)
	}
	fs.starting = true
	fs.mu.Unlock()
	err := fs.serveShards(ctx)
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.starting = false
	if err != nil {
		return fmt.Errorf("vfs: start: %w", err)
	}
	fs.state = serving
	return nil
}

// serveShards is Start's first step. It serves the shard of every share,
// live or being removed, that no failed Start before it served, so each
// installs its persistent opens, then has open state drop the state of every
// share being removed, as its removal's step at a start does, telling each
// holder: no call is admitted yet, so none is left to join. The table raises
// its events meanwhile, which the filesystem delivers as when serving.
func (fs *Filesystem) serveShards(ctx context.Context) error {
	ix := fs.meta.ShareIndex()
	_, shares, err := ix.List(ctx)
	if err != nil {
		return err
	}
	removing, err := ix.Removing(ctx)
	if err != nil {
		return err
	}
	for _, id := range removing {
		sh, _, err := ix.Share(ctx, id)
		if err != nil {
			return err
		}
		shares = append(shares, sh)
	}
	fs.mu.Lock()
	shares = slices.DeleteFunc(shares, func(sh entity.Share) bool { return fs.served[sh.ID] })
	fs.mu.Unlock()
	if err := fs.serve(ctx, shares); err != nil {
		return err
	}
	fs.mu.Lock()
	for _, id := range removing {
		fs.removed[id] = true
	}
	fs.mu.Unlock()
	for _, id := range removing {
		if err := fs.recovery.DropShare(ctx, id, netip.Addr{}); err != nil {
			return err
		}
	}
	return nil
}

// serve raises the incarnation of each share's shard, in one transaction, a
// new grace instance, and has open state serve each share's shard under it.
//
// ponytail: serve raises the incarnations itself, in its own transaction,
// standing in for the node's start transaction, which raises them with the
// node epoch; move the raise there when the node record lands.
func (fs *Filesystem) serve(ctx context.Context, shares []entity.Share) error {
	if len(shares) == 0 {
		return nil
	}
	ids := make([]entity.ShardID, len(shares))
	for i, sh := range shares {
		ids[i] = sh.Shard
	}
	if _, err := fs.meta.Lifecycle().RaiseAtStart(ctx, ids); err != nil {
		return err
	}
	for _, sh := range shares {
		rec, err := fs.meta.Shard(ctx, sh.Shard)
		if err != nil {
			return err
		}
		if err := fs.recovery.ServeShard(ctx, sh.ID, sh.Shard, rec.GraceInstance); err != nil {
			return err
		}
		fs.mu.Lock()
		fs.served[sh.ID] = true
		fs.mu.Unlock()
	}
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
// a built one whose Start was never called or failed. It ends the callbacks
// being delivered and the releases being run and waits for them to return,
// and keeps no queue to flush: a release it ends is pending in the store,
// and the next start's recovery runs it.
// It closes nothing it was handed: the node stops open state, then the
// engine, the event sink and the store, after it.
//
// decision: the lifecycle's close is named Stop, since Close is the Data
// area's close of an open, and *Filesystem carries both.
func (fs *Filesystem) Stop() error {
	fs.mu.Lock()
	if fs.state != built && fs.state != draining || fs.starting {
		defer fs.mu.Unlock()
		return fmt.Errorf("vfs: Stop of a %s filesystem", fs.state)
	}
	if fs.active > 0 {
		defer fs.mu.Unlock()
		return fmt.Errorf("vfs: Stop with %d calls admitted: Drain has not returned", fs.active)
	}
	fs.state = stopped
	fs.cancel()
	fs.mu.Unlock()
	fs.sending.Wait()
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
