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
	sem  *semaphore.Weighted
	refs int
}

func (a *payloadAdmission) acquire(ctx context.Context, id string, exclusive bool) (func(), error) {
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
	weight := int64(1)
	if exclusive {
		weight = math.MaxInt64
	}
	drop := func() {
		a.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(a.entries, id)
		}
		a.mu.Unlock()
	}
	if err := e.sem.Acquire(ctx, weight); err != nil {
		drop()
		return nil, err
	}
	return func() { e.sem.Release(weight); drop() }, nil
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
	ctx, release, err := bs.beginPayloadScope(ctx, payloadIDs, exclusive)
	if err != nil {
		return err
	}
	defer release()
	return fn(ctx)
}

func (bs *Store) beginPayloadScope(ctx context.Context, ids []string, exclusive bool) (context.Context, func(), error) {
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
	var releases []func()
	release := func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
		bs.closeMu.RUnlock()
	}
	for _, id := range ids {
		r, err := bs.admission.acquire(ctx, id, exclusive)
		if err != nil {
			release()
			return ctx, nil, err
		}
		releases = append(releases, r)
	}
	parent, _ := ctx.Value(payloadScopeKey{}).(*payloadScope)
	s := &payloadScope{parent: parent, store: bs, admission: &bs.admission, ids: ids, exclusive: exclusive}
	s.active.Store(true)
	return context.WithValue(ctx, payloadScopeKey{}, s), func() { s.active.Store(false); release() }, nil
}

// enterPayload is the ordinary shared path. Its derived context is passed to
// the syncer so a nested flush reuses admission even with a clone waiting.
func (bs *Store) enterPayload(ctx context.Context, ids ...string) (context.Context, func(), error) {
	return bs.beginPayloadScope(ctx, ids, false)
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
