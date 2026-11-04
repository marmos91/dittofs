// Package engine is the engine: it orders every content operation of the
// shares this node serves across the journals, one per device, and block
// metadata. The filesystem service is its only caller of the content methods;
// the node calls the lifecycle and share methods.
//
// Every byte is held in the journal: nothing is offloaded.
package engine

import (
	"context"
	"fmt"
	"maps"
	"sync"
	"sync/atomic"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// errUnbuilt answers every call whose behaviour is not built yet.
//
// decision: an unbuilt call answers vfserr.ErrUnavailable, wrapped so a test
// tells it from an outage, rather than an error of the engine's own: the
// engine returns only vfserr's values, and a caller maps this one already.
// It holds while nothing composes the engine; every call is built, and this
// removed, before the filesystem service runs over it.
var errUnbuilt = fmt.Errorf("engine: not built: %w", vfserr.ErrUnavailable)

// Engine is the engine of one node.
type Engine struct {
	d        Deps
	settings atomic.Pointer[Settings]
	shares   atomic.Pointer[bindings]       // the share table; swapped whole, read without a lock
	files    *files                         // per-file state
	instance [16]byte                       // the process instance, a verifier input
	sample   func(*binding) vfserr.Verifier // samples a write's verifier, before it is staged
	reads    readSeries
	lost     lostLog

	// mu is the lifecycle mutex: Bind takes it only to read and swap the
	// share table and the fields below, never across I/O, and no content call
	// takes it.
	mu       sync.Mutex
	journals []Journal                   // the journals open, one per device; Start opens them
	busy     map[entity.ShareID]struct{} // the shares with a lifecycle call in progress
	raised   map[entity.ShardID]struct{} // the shards whose incarnation this process raised
	starting bool                        // a Start runs: a lifecycle call answers ErrDelay
	started  bool                        // a Start succeeded
	health   Stats                       // the shares refused at start and the conditions raised

	gcMu            sync.Mutex
	gcs             map[Journal]*groupCommit                                           // each journal's group commit
	gcsClosed       bool                                                               // closeGroupCommits ran: none starts again
	splits          atomic.Int64                                                       // engine_group_commit_splits_total
	stabilityPoints atomic.Int64                                                       // engine_stability_points_total
	pendingApplied  atomic.Int64                                                       // engine_pending_existence_ops: operations committed
	hooks           struct{ taken, captured, drained, written atomic.Pointer[func()] } // a test's seams: after a batch's take, its capture, a drain's read of the ring, and a write's WriteAt
	afterRegister   func()                                                             // a test's hook: run once a clone's registrations are installed
}

// New checks d and builds the engine. It takes no settings, opens nothing and
// starts no goroutine. A missing member of d is refused with an error naming
// it.
func New(d Deps) (*Engine, error) {
	for _, m := range []struct {
		name    string
		missing bool
	}{
		{"Node", d.Node == (entity.NodeID{})},
		{"Devices", len(d.Devices) == 0},
		{"Open", d.Open == nil},
		{"Existence", d.Existence == nil},
		{"Content", d.Content == nil},
		{"Lifecycle", d.Lifecycle == nil},
		{"Holds", d.Holds == nil},
		{"Clock", d.Clock == nil},
		{"Logger", d.Logger == nil},
		{"Tracer", d.Tracer == nil},
	} {
		if m.missing {
			return nil, fmt.Errorf("engine: Deps.%s is missing", m.name)
		}
	}
	d.Logger = d.Logger.With("component", "engine")

	e := &Engine{
		d:        d,
		files:    newFiles(d.Clock),
		instance: instanceID(),
		busy:     map[entity.ShareID]struct{}{},
		raised:   map[entity.ShardID]struct{}{},
	}
	e.sample = e.verifier

	return e, nil
}

// Apply hands the engine its node-level settings: first before Start, then on
// every change. An invalid one is refused and the engine keeps its last.
func (e *Engine) Apply(s Settings) error {
	switch {
	case s.ExistenceAge <= 0:
		return fmt.Errorf("engine: existence_age %v is not positive", s.ExistenceAge)
	case s.GroupCommitMaxFiles <= 0:
		return fmt.Errorf("engine: group_commit.max_files %d is not positive", s.GroupCommitMaxFiles)
	case s.CloneMaxLen <= 0:
		return fmt.Errorf("engine: clone_max_len %d is not positive", s.CloneMaxLen)
	}
	e.settings.Store(&s)
	return nil
}

// errStarted refuses a Start while one runs or after one succeeded.
var errStarted = fmt.Errorf("engine: already started: %w", vfserr.ErrInvalid)

// errDevices refuses a Start over more than one journal device.
var errDevices = fmt.Errorf("engine: more than one journal device, and no shard record names its share's journal: %w", errUnbuilt)

// Start runs the start steps over shares, each complete before the next, and
// serves the shares once they have all run (RFC 8 §2.5). A step that fails
// closes in reverse what earlier steps opened and returns its error, so the
// engine is serving or as built, and Start may run again. A share refused
// at step 1 does not fail Start: it is not attached, and Stats names it and
// its device. Start runs once: a second, or one while one runs, is refused.
func (e *Engine) Start(ctx context.Context, shares []Share) error {
	e.mu.Lock()

	if e.started || e.starting {
		e.mu.Unlock()

		return errStarted
	}

	e.starting = true
	e.mu.Unlock()

	o, err := e.start(ctx, shares)
	if err == nil {
		// Each served share's binding takes its journal's group commit once
		// every start step succeeded, so a failed Start leaves none running.
		for _, b := range o.table {
			if !b.removed {
				b.gc = e.groupCommitFor(b.j)
			}
		}
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	e.starting = false

	if err != nil {
		return err
	}

	t := make(bindings, len(e.table())+len(o.table))
	maps.Copy(t, e.table())
	maps.Copy(t, o.table)
	e.shares.Store(&t)

	if o.journal != nil {
		e.journals = []Journal{o.journal}
	}

	e.health = o.stats
	e.started = true

	return nil
}

// start runs the start steps, closing in reverse what they opened when one
// fails, and returns what Start publishes.
func (e *Engine) start(ctx context.Context, shares []Share) (opened, error) {
	// ponytail: Start serves one journal device only, as nothing the engine
	// reads names the journal a share lives on, so it cannot tell which
	// device's journal gives a share its handle, counts in its floor or
	// forgets its tag. The ceiling is a node with more than one device not
	// starting at all. Serve every device once the shard record carries its
	// primary's journal identity and Start matches it against the identity
	// JournalID answers for each device.
	if len(e.d.Devices) != 1 {
		return opened{}, errDevices
	}

	var u undo

	o, err := e.openJournal(ctx, shares, &u)
	if err != nil {
		return opened{}, u.unwind(err)
	}

	return o, nil
}

// Close stops the engine and closes what it opened, in reverse.
func (e *Engine) Close(ctx context.Context) error { return errUnbuilt }

// Unbind admits nothing new for the share and joins what it has in flight.
// While Start runs it answers ErrDelay.
func (e *Engine) Unbind(ctx context.Context, id entity.ShareID, keepHolds bool) error {
	if err := e.whileStarting(); err != nil {
		return err
	}

	return errUnbuilt
}

// Drop closes the share's handles and forgets its tag in every journal.
// While Start runs it answers ErrDelay.
func (e *Engine) Drop(ctx context.Context, id entity.ShareID) error {
	if err := e.whileStarting(); err != nil {
		return err
	}

	return errUnbuilt
}

// whileStarting is ErrDelay with DelayFrozen while Start runs, the shares not
// attached yet (RFC 17 §4.3), and nil otherwise.
func (e *Engine) whileStarting() error {
	e.mu.Lock()

	defer e.mu.Unlock()

	if e.starting {
		return &vfserr.Delay{Cause: vfserr.DelayFrozen}
	}

	return nil
}

// Stats reports the engine's counters and conditions: before Start, none.
func (e *Engine) Stats() Stats {
	e.mu.Lock()

	defer e.mu.Unlock()

	return e.health.clone()
}
