package engine

import (
	"context"
	"errors"
	"math"
	"slices"
	"sync"
	"sync/atomic"

	"golang.org/x/sync/semaphore"
)

// payloadAdmission is shared by an engine and its background flusher. Ordinary
// operations share admission; replacing a payload excludes them until both the
// manifest and local bytes describe the replacement. Entries include waiters
// in their reference count, so retiring an entry cannot split its lock.
type payloadAdmission struct {
	mu      sync.Mutex
	entries map[string]*payloadEntry
}

type payloadEntry struct {
	sem   *semaphore.Weighted
	refs  int
	epoch atomic.Uint64
}

func (a *payloadAdmission) pin(id string) (*payloadEntry, func()) {
	a.mu.Lock()
	if a.entries == nil {
		a.entries = make(map[string]*payloadEntry)
	}
	e := a.entries[id]
	if e == nil {
		e = &payloadEntry{sem: semaphore.NewWeighted(math.MaxInt64)}
		a.entries[id] = e
	}
	e.refs++
	a.mu.Unlock()
	drop := func() {
		a.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(a.entries, id)
		}
		a.mu.Unlock()
	}
	return e, drop
}

var errPayloadBusy = errors.New("payload is busy")

type payloadHold struct {
	entry   *payloadEntry
	release func()
}

func (a *payloadAdmission) hold(ctx context.Context, id string, exclusive, try bool) (payloadHold, error) {
	e, drop := a.pin(id)
	weight := int64(1)
	if exclusive {
		weight = math.MaxInt64
	}
	if try {
		if err := ctx.Err(); err != nil {
			drop()
			return payloadHold{}, err
		}
		if !e.sem.TryAcquire(weight) {
			drop()
			return payloadHold{}, errPayloadBusy
		}
		return payloadHold{entry: e, release: func() { e.sem.Release(weight); drop() }}, nil
	}
	if err := e.sem.Acquire(ctx, weight); err != nil {
		drop()
		return payloadHold{}, err
	}
	return payloadHold{entry: e, release: func() { e.sem.Release(weight); drop() }}, nil
}

func (a *payloadAdmission) acquire(ctx context.Context, id string, exclusive bool) (func(), error) {
	h, err := a.hold(ctx, id, exclusive, false)
	return h.release, err
}

// observe pins the entry without owning admission. Its epoch changes when an
// exclusive operation finishes, so a warm plan can detect replacement while
// allowing that replacement to proceed between its workers.
func (a *payloadAdmission) observe(ctx context.Context, id string) (func() uint64, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if a == nil {
		return func() uint64 { return 0 }, func() {}, nil
	}
	e, drop := a.pin(id)
	return e.epoch.Load, drop, nil
}

// ObservePayload retains a replacement epoch without retaining payload or
// lifecycle admission. Read the returned version under shared admission along
// with the state it describes, and release the observer when its work ends.
func (bs *Store) ObservePayload(ctx context.Context, payloadID string) (func() uint64, func(), error) {
	return bs.admission.observe(ctx, payloadID)
}

type payloadScopeKey struct{}
type payloadScope struct {
	parent    *payloadScope
	store     *Store
	admission *payloadAdmission
	ids       []string
	exclusive bool
	active    atomic.Bool
}

func scopeFor(ctx context.Context, a *payloadAdmission) *payloadScope {
	s, _ := ctx.Value(payloadScopeKey{}).(*payloadScope)
	for ; s != nil; s = s.parent {
		if s.admission == a && s.active.Load() {
			return s
		}
	}
	return nil
}

// WithPayloadScope keeps the store alive and admits an entire operation,
// including its metadata phase. The callback must use the supplied context for
// nested engine calls and must not retain it or leave child work running.
// Distinct payload IDs are acquired in sorted order. Nested calls may reuse
// owned IDs, but cannot add IDs or upgrade shared ownership to exclusive.
// No metadata transaction or journal lock may be held when entering a scope.
func (bs *Store) WithPayloadScope(ctx context.Context, payloadIDs []string, exclusive bool, fn func(context.Context) error) error {
	ctx, release, err := bs.beginPayloadScope(ctx, payloadIDs, exclusive, false)
	if err != nil {
		return err
	}
	defer release()
	return fn(ctx)
}

// TryWithPayloadScope runs fn only if every payload is immediately available.
// A busy payload releases earlier acquisitions without queueing a writer: slow
// shared uploads therefore cannot make this caller block later reads or writes.
// The bool reports whether fn ran; an error from fn still reports true.
func (bs *Store) TryWithPayloadScope(ctx context.Context, payloadIDs []string, exclusive bool, fn func(context.Context) error) (bool, error) {
	ctx, release, err := bs.beginPayloadScope(ctx, payloadIDs, exclusive, true)
	if errors.Is(err, errPayloadBusy) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer release()
	return true, fn(ctx)
}

func (bs *Store) beginPayloadScope(ctx context.Context, ids []string, exclusive, try bool) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return ctx, nil, err
	}
	if s := scopeFor(ctx, &bs.admission); s != nil {
		if exclusive && !s.exclusive {
			return ctx, nil, errors.New("cannot upgrade a payload scope")
		}
		for _, id := range ids {
			if !slices.Contains(s.ids, id) {
				return ctx, nil, errors.New("cannot expand a payload scope")
			}
		}
		return ctx, func() {}, nil
	}
	// Lifecycle precedes payload admission. Close cannot start teardown while
	// an operation waits for a payload, and owned calls never recursively take
	// closeMu.RLock while Close is queued as a writer.
	if err := bs.enter(); err != nil {
		return ctx, nil, err
	}
	ids = slices.Clone(ids)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	var holds []payloadHold
	release := func() {
		for i := len(holds) - 1; i >= 0; i-- {
			holds[i].release()
		}
		bs.closeMu.RUnlock()
	}
	for _, id := range ids {
		h, err := bs.admission.hold(ctx, id, exclusive, try)
		if err != nil {
			release()
			return ctx, nil, err
		}
		holds = append(holds, h)
	}
	if err := ctx.Err(); err != nil {
		release()
		return ctx, nil, err
	}
	parent, _ := ctx.Value(payloadScopeKey{}).(*payloadScope)
	s := &payloadScope{parent: parent, store: bs, admission: &bs.admission, ids: ids, exclusive: exclusive}
	s.active.Store(true)
	return context.WithValue(ctx, payloadScopeKey{}, s), func() {
		s.active.Store(false)
		if exclusive {
			for _, h := range holds {
				h.entry.epoch.Add(1)
			}
		}
		release()
	}, nil
}

// enterPayload is the ordinary shared path. Its derived context is passed to
// the syncer so a nested flush reuses admission even with a clone waiting.
func (bs *Store) enterPayload(ctx context.Context, ids ...string) (context.Context, func(), error) {
	return bs.beginPayloadScope(ctx, ids, false, false)
}

// enter is the syncer's admission path. Workers are joined by RemoteSync.Close
// and therefore do not take the engine lifecycle lock: Close already owns that
// lock while it stops workers and performs the final synchronous drain.
func (a *payloadAdmission) enter(ctx context.Context, id string) (func(), error) {
	if a == nil {
		return func() {}, nil
	}
	if s := scopeFor(ctx, a); s != nil {
		if !slices.Contains(s.ids, id) {
			return nil, errors.New("flush outside owned payload scope")
		}
		return func() {}, nil
	}
	return a.acquire(ctx, id, false)
}

// enterContext joins an outer lifecycle scope without recursively locking it.
func (bs *Store) enterContext(ctx context.Context) (func(), error) {
	if scopeFor(ctx, &bs.admission) != nil {
		return func() {}, nil
	}
	if err := bs.enter(); err != nil {
		return nil, err
	}
	return bs.closeMu.RUnlock, nil
}
