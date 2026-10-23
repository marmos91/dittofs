package openstate

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/openstate/sequence"
)

// Table is the open state of every file this node is the primary of. Each
// file's state lives in its own entry, created with the file's first piece of
// state and freed with its last, so a file with none costs nothing.
type Table struct {
	d Deps

	// files is split 256 ways by a FileID's first byte, which is random, so
	// steps on different files rarely share a stripe's mutex. A stripe's mutex
	// is held only to find, add or free an entry, never while a step runs.
	files [256]fileStripe

	// idx guards the state references, the open IDs, the counts per Via and
	// the files holding a poll waiter. It is taken after a file's step, never
	// before one, and only to add, remove or look up one of them.
	idx   sync.Mutex
	refs  map[refKey]holder
	byID  map[entity.OpenID]refKey
	via   map[entity.FileID]int      // the opens made through each directory
	polls map[entity.FileID]struct{} // the files holding a poll waiter, which the timer lapses

	// smu guards the owner sequences. It is taken under a file's step or
	// under nothing, and nothing is taken under it.
	smu      sync.Mutex
	seqs     sequence.Book
	inflight map[sequence.Owner]struct{} // owners with a numbered request between its check and its answer

	// cstep is the client step: every change to a client record, and every
	// lease armed from one, runs under it, so a confirm, an expiry and a
	// renewal that reads the record never interleave. It is never taken
	// under a file's step.
	//
	// ponytail: one step for every client, held across its record write, so
	// confirms and expiries of all clients serialise on the store. Stripe it
	// by client name and ID when mounts or expiries queue on it.
	cstep sync.Mutex

	// cmu guards the client area: the clients this node serves, the
	// proposals it answered and the lifecycle. It is held only to read or
	// change those maps, never across a store call or a Sink call, and may
	// be taken under idx, never the other way.
	cmu     sync.Mutex
	leases  map[entity.ClientID]time.Duration // the Elapsed reading each client's lease ends at; zero: no lease
	known   map[entity.ClientID]known         // what the table reads of each client's record, with its lease
	pending map[nameKey]pending
	sources map[netip.Prefix]int // pending proposals per source
	sink    Sink                 // nil before Start and after Stop
	stopped bool
	stop    func()

	order Order          // set by Start, before the timer runs
	timer sync.WaitGroup // the timer and the steps it starts
}

type fileStripe struct {
	mu      sync.Mutex
	entries map[entity.FileID]*entry
}

// entry is one file's state. Its mutex is the file's open-state step: every
// call that reads or changes the file's state runs under it.
type entry struct {
	file   entity.FileID
	mu     sync.Mutex
	users  int // steps running or waiting on this entry, guarded by its file stripe's mutex
	states int // pieces of state held, guarded by mu

	opens    map[uint64]*open      // by reference token
	lstates  map[uint64]*lockState // by reference token
	waiters  []*LockWaiter         // in Arrived order
	pending  *entity.DeleteRecord  // the file is delete pending; nil: not
	unlinked bool                  // an unlink made the opens durable: the file has no entry, and every new open is written durable
}

// known is what the table holds of a client's record while it is current
// here.
type known struct {
	recovery entity.RecoveryKind
	owner    entity.Principal // the owner that may act on all its state; zero unless it protects it
	shards   []ShardID        // shards seen listed in its record
}

// New builds an empty table. It starts nothing: Start does.
func New(d Deps) (*Table, error) {
	if d.Records == nil || d.Clock == nil || d.Rand == nil {
		return nil, errors.New("openstate: a table needs a store, a clock and a source of random bits")
	}
	t := &Table{
		d: d, refs: map[refKey]holder{}, byID: map[entity.OpenID]refKey{}, via: map[entity.FileID]int{}, polls: map[entity.FileID]struct{}{}, seqs: sequence.Book{}, inflight: map[sequence.Owner]struct{}{},
		leases: map[entity.ClientID]time.Duration{}, known: map[entity.ClientID]known{}, pending: map[nameKey]pending{}, sources: map[netip.Prefix]int{},
	}
	for i := range t.files {
		t.files[i].entries = map[entity.FileID]*entry{}
	}
	return t, nil
}

// step runs fn under file f's step, creating the file's entry if it has none
// and freeing it afterwards if fn left it holding no state.
func (t *Table) step(f entity.FileID, fn func(e *entry) error) error {
	fs := &t.files[f[0]]
	fs.mu.Lock()
	e := fs.entries[f]
	if e == nil {
		e = &entry{file: f}
		fs.entries[f] = e
	}
	e.users++
	fs.mu.Unlock()

	e.mu.Lock()
	err := fn(e)
	e.mu.Unlock()

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if e.users--; e.users > 0 {
		return err
	}
	// No step holds or waits on the entry, so its mutex is free and nothing
	// can change its state while it is read here.
	e.mu.Lock()
	if e.states == 0 {
		delete(fs.entries, f)
	}
	e.mu.Unlock()
	return err
}

// Clients is the view of client records and leases.
func (t *Table) Clients() Clients { return clients{t} }

// sweepEvery is how often lapsed leases, proposals and poll waiters are
// looked for.
//
// ponytail: each sweep scans every lease and proposal, O(clients), and every
// file holding a poll waiter, and a client or a poll waiter is expired up to
// one interval after its deadline. Replace it with the deadline heap, one
// (kind, ref, generation) entry per deadline, when a profile shows the sweep
// or a deadline must fire on the instant.
const sweepEvery = 10 * time.Second

// Start starts the timer and takes the service's Sink and admission lock.
// Before Start and after Stop every mutating call is ErrStopped, and a table
// starts once. The timer outlives ctx: only Stop ends it.
//
// ponytail: Start arms no lease, so after a restart an NFSv4 client that
// never renews keeps its record, its name and its shard marks until it
// renews, which may be never; one that renews is adopted with a full period.
// Arm a lease for every NFSv4 record this node owns here instead, which
// needs a store call listing a node's clients, added as a store follow-up
// before stage 2's service calls Start.
func (t *Table) Start(ctx context.Context, sink Sink, order Order) error {
	if sink == nil || order == nil {
		return errors.New("openstate: Start needs a Sink and an Order")
	}
	t.cmu.Lock()
	defer t.cmu.Unlock()
	if t.sink != nil || t.stopped {
		return errors.New("openstate: table already started")
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	t.order = order
	t.timer.Go(func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.d.Clock.After(sweepEvery):
				t.sweep(ctx)
			}
		}
	})
	t.sink, t.stop = sink, func() { cancel(); t.timer.Wait() }
	return nil
}

// Stop stops the timer, and waits for the steps it started. Open state is left as it is, for the next
// start's grace.
//
// decision: Stop waits for the timer, not for calls already past running():
// such a call still finishes and may raise ClientEnded after Stop returns.
// It holds because the node stops the service, the table's only caller,
// first. Count in-flight calls and wait for them if a caller is found that
// outlives the service.
func (t *Table) Stop() {
	t.cmu.Lock()
	stop := t.stop
	t.sink, t.stop, t.stopped = nil, nil, true
	t.cmu.Unlock()
	if stop != nil {
		stop()
	}
}

// current reports whether client c is current here: known, and its lease, if
// it holds one, not ended. The caller holds idx or nothing.
func (t *Table) current(c entity.ClientID) bool {
	t.cmu.Lock()
	defer t.cmu.Unlock()
	_, ok := t.known[c]
	end := t.leases[c]
	return ok && (end == 0 || t.d.Clock.Elapsed() < end)
}

// running is the Sink, or ErrStopped before Start and after Stop.
func (t *Table) running() (Sink, error) {
	t.cmu.Lock()
	defer t.cmu.Unlock()
	if t.sink == nil {
		return nil, ErrStopped
	}
	return t.sink, nil
}
