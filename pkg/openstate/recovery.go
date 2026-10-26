package openstate

import (
	"context"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
)

// Recovery is the view of a client's references with no file handle; it runs
// at the primary of the shard each reference names.
type Recovery interface {
	// TestState answers each reference as a call naming it would be
	// answered: nil, or why it is refused. It changes nothing.
	TestState(ctx context.Context, who Caller, s []StateRef) []error
	// FreeState frees a revoked reference from the client's revoked set, or
	// drops a lock state holding no locks. A lock state holding some, a live
	// open or a live grant is ErrLocksHeld. The waiters a drop frees are
	// served under the file's admission lock, which the table takes itself:
	// the service must not hold it when calling.
	FreeState(ctx context.Context, who Caller, s StateRef) error
}

type recovery struct{ t *Table }

func (v recovery) TestState(ctx context.Context, who Caller, s []StateRef) []error {
	errs := make([]error, len(s))
	for i, ref := range s {
		_, errs[i] = v.t.resolve(who.Client, entity.FileID{}, ref)
	}
	return errs
}

func (v recovery) FreeState(ctx context.Context, who Caller, s StateRef) error {
	t := v.t
	sink, err := t.running()
	if err != nil {
		return err
	}
	k := refKey{s.Shard, s.Token}
	t.idx.Lock()
	h, err := t.resolveLocked(who.Client, entity.FileID{}, s)
	if _, ok := err.(*RevokedError); ok {
		delete(t.revoked, k)
		t.idx.Unlock()
		return nil
	}
	t.idx.Unlock()
	switch {
	case err != nil:
		return err
	case h.typ != StateLock:
		return ErrLocksHeld
	}
	// A reference names no file, so the service holds no admission lock for
	// it: the waiters the release frees are served after it, under the
	// file's admission lock, as the timer serves them.
	out := outbox{timer: true}
	err = t.step(h.file, func(e *entry) error {
		ls := e.lstates[s.Token]
		switch {
		case ls == nil || ls.ref.Shard != s.Shard:
			return ErrBadState
		case len(ls.locks) > 0:
			return ErrLocksHeld
		}
		t.dropLockState(e, ls)
		t.serve(e, &out)
		return nil
	})
	out.raise(t, sink)
	t.serveFiles(sink, out.serve)
	return err
}
