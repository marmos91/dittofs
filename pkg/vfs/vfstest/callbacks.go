package vfstest

import (
	"context"
	"sync"

	"github.com/marmos91/dittofs/pkg/vfs"
)

// Callback is one callback as RecordingCallbacks saw it: the method's name
// and its arguments, the context left out.
type Callback struct {
	Method string
	Args   []any
}

// RecordingCallbacks is a client's callback path that records every
// callback. Answer, when set, is each one's error: a client that stalls is
// one that blocks until ctx ends, one that fails returns an error. Refuse is
// LockWaitOver's answer that the client refused a grant. Both are set before
// the path is registered.
type RecordingCallbacks struct {
	Answer func(ctx context.Context, c Callback) error
	Refuse bool

	mu    sync.Mutex
	calls []Callback
}

var _ vfs.Callbacks = (*RecordingCallbacks)(nil)

// Calls is every callback so far, in order.
func (r *RecordingCallbacks) Calls() []Callback {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Callback(nil), r.calls...)
}

func (r *RecordingCallbacks) record(ctx context.Context, method string, args ...any) error {
	c := Callback{Method: method, Args: args}
	r.mu.Lock()
	r.calls = append(r.calls, c)
	r.mu.Unlock()
	if r.Answer == nil {
		return nil
	}
	return r.Answer(ctx, c)
}

func (r *RecordingCallbacks) RecallGrant(ctx context.Context, g vfs.CachingGrant, to vfs.GrantKind) error {
	return r.record(ctx, "RecallGrant", g, to)
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
