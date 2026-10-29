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

	// idx guards the state references, the open IDs, the counts per Via,
	// the files holding a poll waiter, the revoked set and what the grants
	// count. It is taken after a file's step, never before one, and only to
	// add, remove or look up one of them.
	idx       sync.Mutex
	refs      map[refKey]holder
	byID      map[entity.OpenID]refKey
	via       map[entity.FileID]int           // the opens made through each directory
	polls     map[entity.FileID]struct{}      // the files holding a poll waiter, which the timer lapses
	revoked   map[refKey]revokedRef           // every client's revoked set, until each reference is freed
	granted   map[entity.ClientID]int         // the grants each client holds
	keys      map[clientKey]keyUse            // each lease key held by an open, from its first open to its last close
	recalling map[entity.FileID]struct{}      // the files holding a recall handed to the Sink, which the timer lapses
	recalled  map[entity.FileID]time.Duration // the Elapsed reading until which a file is offered no grant, after a recall ended
	unsent    int                             // recalls started and not yet handed to the Sink
	parked    map[entity.FileID]struct{}      // the files holding a disconnected open or an unsaved record, which the timer visits
	ended     map[sessionKey]time.Duration    // the sessions each client ended here, from the Elapsed reading of their end, for a lease period
	held      map[heldKey]int                 // what each client holds here of each capped kind
	owners    map[ownerKey]int                // the lock states of each lock owner, which holds one per open or file it locks under

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
	// proposals it answered, the shards it serves and the lifecycle. It is
	// held only to read or change those maps, never across a store call or
	// a Sink call, and may be taken under idx, never the other way.
	cmu     sync.Mutex
	leases  map[entity.ClientID]time.Duration // the Elapsed reading each client's lease ends at; zero: no lease
	known   map[entity.ClientID]known         // what the table reads of each client's record, with its lease
	pending map[nameKey]pending
	sources map[netip.Prefix]int              // pending proposals per source
	fenced  map[entity.ClientID]time.Duration // the Elapsed reading until which each client is offered no grant
	adopted map[entity.ClientID]struct{}      // the clients install made known here, until they come back or hold nothing
	shards  map[ShardID]*served
	sink    Sink // nil before Start and after Stop
	stopped bool
	stop    func()

	order Order          // set by Start, before the timer runs
	timer sync.WaitGroup // the timer and the steps it starts
}

// capped is a kind of state each client holds a bounded count of here.
type capped uint8

const (
	cappedOpens capped = iota
	cappedRanges
	cappedOwners
	cappedWaiters
	cappedWatches
)

// perClient is the most of each capped kind one client holds here.
//
// decision: 16,384 opens, 65,536 lock ranges, 4,096 lock owners, 1,024
// waiters and 1,024 watches per client, none measured. A request past one
// is refused ErrDelay before it changes anything, so one client cannot spend
// what the node holds for every client. Each is a variable only so a case
// can reach it without building it whole. Make them settings if a measured
// client is refused below them.
var perClient = [...]int{cappedOpens: 16384, cappedRanges: 65536, cappedOwners: 4096, cappedWaiters: 1024, cappedWatches: 1024}

type heldKey struct {
	client entity.ClientID
	kind   capped
}

type ownerKey struct {
	client entity.ClientID
	name   string
}

// chargeLocked adds n of kind k to what client c holds; with refuse set, a
// count past the cap is ErrDelay and changes nothing. The caller holds idx.
// State of no client is counted against none.
func (t *Table) chargeLocked(c entity.ClientID, k capped, n int, refuse bool) error {
	if c == 0 || n == 0 {
		return nil
	}
	key := heldKey{c, k}
	if refuse && n > 0 && t.held[key]+n > perClient[k] {
		return &DelayError{DelayResources}
	}
	if t.held[key] += n; t.held[key] <= 0 {
		delete(t.held, key)
	}
	return nil
}

// full reports whether n more of kind k would take client c past its cap.
func (t *Table) full(c entity.ClientID, k capped, n int) bool {
	t.idx.Lock()
	defer t.idx.Unlock()
	return c != 0 && n > 0 && t.held[heldKey{c, k}]+n > perClient[k]
}

// charge is chargeLocked, taking idx.
func (t *Table) charge(c entity.ClientID, k capped, n int, refuse bool) error {
	t.idx.Lock()
	defer t.idx.Unlock()
	return t.chargeLocked(c, k, n, refuse)
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

	opens   map[uint64]*open      // by reference token
	lstates map[uint64]*lockState // by reference token
	waiters []*LockWaiter         // in arrival order, which serves them
	grants  map[uint64]*grant     // by reference token
	pending *entity.DeleteRecord  // the file is delete pending; nil: not
	// pendingUnsaved is set while pending differs from what is written for
	// a persistent open, which the timer writes again.
	pendingUnsaved bool
	unlinked       bool // an unlink made the opens durable: the file has no entry, and every new open is written durable
}

// known is what the table holds of a client's record while it is current
// here.
type known struct {
	recovery entity.RecoveryKind
	owner    entity.Principal   // the owner that may act on all its state; zero unless it protects it
	down     bool               // its back channel is lost: it is offered no grant until it rebinds
	shards   []entity.ShardMark // its record's marks, as last read or written here
}

// New builds an empty table. It starts nothing: Start does.
func New(d Deps) (*Table, error) {
	if d.Records == nil || d.Clock == nil || d.Rand == nil {
		return nil, errors.New("openstate: a table needs a store, a clock and a source of random bits")
	}
	t := &Table{
		d: d, refs: map[refKey]holder{}, byID: map[entity.OpenID]refKey{}, via: map[entity.FileID]int{}, polls: map[entity.FileID]struct{}{},
		revoked: map[refKey]revokedRef{}, granted: map[entity.ClientID]int{}, keys: map[clientKey]keyUse{}, recalling: map[entity.FileID]struct{}{}, recalled: map[entity.FileID]time.Duration{}, parked: map[entity.FileID]struct{}{}, ended: map[sessionKey]time.Duration{},
		adopted: map[entity.ClientID]struct{}{}, held: map[heldKey]int{}, owners: map[ownerKey]int{},
		seqs: sequence.Book{}, inflight: map[sequence.Owner]struct{}{},
		leases: map[entity.ClientID]time.Duration{}, known: map[entity.ClientID]known{}, pending: map[nameKey]pending{}, sources: map[netip.Prefix]int{}, fenced: map[entity.ClientID]time.Duration{},
		shards: map[ShardID]*served{},
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

// Grants is the view of caching grants.
func (t *Table) Grants() Grants { return grants{t} }

// Recovery is the view of recovery: the shards served and their grace, and
// references tested and freed.
func (t *Table) Recovery() Recovery { return recovery{t} }

// sweepEvery is how often lapsed leases, proposals and poll waiters are
// looked for.
//
// ponytail: each sweep scans every lease and proposal, O(clients), and every
// file holding a poll waiter or a recall handed to the Sink, and a client, a
// poll waiter or a recall is expired up to one interval after its deadline:
// a 35 s recall up to 45 s, still within the 60 s an SMB adapter holds the
// request it delays. Replace it with the deadline heap, one
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
