package vfstest

import (
	"context"
	"slices"
	"sync"

	"github.com/cubbit/dittofs/pkg/vfs"
)

// Callback is one callback as RecordingCallbacks saw it: the method's name
// and its arguments, the context left out. Session is, for a RecallGrant,
// the session it was sent on: the one named, or the one picked from the
// sessions signed in; zero for one naming no session and no openers, or one
// sent nowhere.
type Callback struct {
	Method  string
	Args    []any
	Session vfs.SessionRef
}

// RecordingCallbacks is a client's callback path that records every
// callback. Answer, when set, is each one's error: a client that stalls is
// one that blocks until ctx ends, one that fails returns an error. Refuse is
// LockWaitOver's answer that the client refused a grant. Both are set before
// the path is registered. SignIn keeps the adapter's own session table, from
// which a RecallGrant naming no session but openers picks its session.
type RecordingCallbacks struct {
	Answer func(ctx context.Context, c Callback) error
	Refuse bool

	mu       sync.Mutex
	calls    []Callback
	sessions map[vfs.SessionRef]vfs.Principal
}

// SignIn records session s of the client as signed in now as p, replacing
// what it was signed in as before: a re-authentication, a guest's included,
// which signs in as the zero principal.
func (r *RecordingCallbacks) SignIn(s vfs.SessionRef, p vfs.Principal) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sessions == nil {
		r.sessions = map[vfs.SessionRef]vfs.Principal{}
	}
	r.sessions[s] = p
}

// SignOff forgets session s.
func (r *RecordingCallbacks) SignOff(s vfs.SessionRef) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, s)
}

// pick is the lowest-numbered session signed in as one of openers, or zero.
func (r *RecordingCallbacks) pick(openers []vfs.Principal) vfs.SessionRef {
	r.mu.Lock()
	defer r.mu.Unlock()
	var got vfs.SessionRef
	for s, p := range r.sessions {
		if !p.IsZero() && slices.Contains(openers, p) && (got == vfs.SessionRef{} || s.ID < got.ID) {
			got = s
		}
	}
	return got
}

var _ vfs.Callbacks = (*RecordingCallbacks)(nil)

// Calls is every callback so far, in order.
func (r *RecordingCallbacks) Calls() []Callback {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Callback(nil), r.calls...)
}

func (r *RecordingCallbacks) record(ctx context.Context, method string, args ...any) error {
	return r.recordOn(ctx, vfs.SessionRef{}, method, args...)
}

func (r *RecordingCallbacks) recordOn(ctx context.Context, s vfs.SessionRef, method string, args ...any) error {
	c := Callback{Method: method, Args: args, Session: s}
	r.mu.Lock()
	r.calls = append(r.calls, c)
	r.mu.Unlock()
	if r.Answer == nil {
		return nil
	}
	return r.Answer(ctx, c)
}

// RecallGrant records the recall with the session it goes on: s, or with s
// zero and openers given, the one pick finds, none leaving it unsent.
func (r *RecordingCallbacks) RecallGrant(ctx context.Context, s vfs.SessionRef, openers []vfs.Principal, g vfs.CachingGrant, to vfs.GrantKind) (bool, error) {
	on, picks := s, s == (vfs.SessionRef{}) && len(openers) > 0
	if picks {
		on = r.pick(openers)
	}
	err := r.recordOn(ctx, on, "RecallGrant", s, openers, g, to)
	return err == nil && (!picks || on != (vfs.SessionRef{})), err
}

func (r *RecordingCallbacks) Notify(ctx context.Context, w vfs.WatchID, changes []vfs.Change) error {
	return r.record(ctx, "Notify", w, changes)
}

// Revoked has no answer to give, so Answer's error is dropped; a stall still
// holds the call.
func (r *RecordingCallbacks) Revoked(ctx context.Context, what vfs.Revocation) {
	_ = r.record(ctx, "Revoked", what)
}

func (r *RecordingCallbacks) LockWaitOver(ctx context.Context, w vfs.LockWaiter, granted bool) (bool, error) {
	err := r.record(ctx, "LockWaitOver", w, granted)
	return r.Refuse, err
}

func (r *RecordingCallbacks) RecallAny(ctx context.Context, keep int) error {
	return r.record(ctx, "RecallAny", keep)
}
