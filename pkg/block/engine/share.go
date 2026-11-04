package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync/atomic"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// errNotStarted answers a Bind while no journal is open.
var errNotStarted = fmt.Errorf("engine: no journal open: %w", vfserr.ErrUnavailable)

// errRemoved answers a content call or a Bind naming a share being removed or
// retired (RFC 8 §12.1).
var errRemoved = fmt.Errorf("engine: the share is being removed: %w", vfserr.ErrStale)

// binding is one bound share's storage: the share as it was attached, its
// journal and its handle there, its state, and the incarnation its attach
// raise wrote. A removed binding is a share being removed that start found
// past its offload step: it holds the share's ID and tag and no journal or
// handle.
type binding struct {
	share       Share // as attached: Limit is the limit the handle uses
	j           Journal
	h           Handle
	state       atomic.Uint32 // a ShareState: a state change stores it and swaps nothing
	incarnation uint64        // the shard's incarnation the attach raise wrote, for the write verifier
	primary     primary       // the verifier's other inputs from the shard's primary
	gc          *groupCommit  // the group commit of j
	removed     bool
}

// bindings is the share table, read on every content call without a lock. A
// removed binding holds no journal or handle: a loop over the table skips it.
//
// decision: the share table is copy-on-write: a share's attach or removal
// copies the whole table, up to 10⁴ entries, once per change, so the hot path
// reads it without a lock. Overturned if share churn is measured in changes
// per second.
type bindings = map[entity.ShareID]*binding

// bindingOf is the binding of share s, read from the share table without a
// lock; a share with no binding is a Delay with DelayFrozen, its binding
// still to come, and a share with a removed binding is ErrStale. Neither
// touches a journal.
func (e *Engine) bindingOf(s entity.ShareID) (*binding, error) {
	b := e.table()[s]

	switch {
	case b == nil:
		return nil, &vfserr.Delay{Cause: vfserr.DelayFrozen}
	case b.removed:
		return nil, errRemoved
	}

	return b, nil
}

// table is the share table now, nil before the first attach.
func (e *Engine) table() bindings {
	if t := e.shares.Load(); t != nil {
		return *t
	}

	return nil
}

// Bind attaches a share the engine does not serve, above its floor, or
// applies a state change to one it serves (RFC 8 §2.3, §2.5). An invalid
// share is refused before any I/O, and the binding it had is kept. A second
// lifecycle call on a share while one runs answers ErrDelay, which its caller
// retries, and so does a Bind while Start runs, the share not attached yet; a
// share being removed answers ErrStale. The lifecycle mutex is held only to
// read and swap the table, never across I/O.
func (e *Engine) Bind(ctx context.Context, s Share) error {
	if err := validShare(s); err != nil {
		return err
	}

	e.mu.Lock()
	_, busy := e.busy[s.ID]
	b := e.table()[s.ID]

	switch {
	case e.starting:
		e.mu.Unlock()

		return &vfserr.Delay{Cause: vfserr.DelayFrozen}
	case busy:
		e.mu.Unlock()

		return &vfserr.Delay{Cause: vfserr.DelayConflict}
	case b != nil && b.removed:
		e.mu.Unlock()

		return errRemoved
	case b != nil:
		err := b.change(s)
		e.mu.Unlock()

		if err != nil {
			return err
		}

		// decision: a served share whose journal limit changed keeps the
		// limit its handle opened with, and the change is logged: the
		// journal gives a handle no call that changes its limit (RFC 1 §3),
		// so the new limit applies only when the share is bound again. Until
		// the RFC adds that call, a lower new limit lets the share grow in
		// the journal up to the old one meanwhile.
		if s.Limit != b.share.Limit {
			e.d.Logger.Warn("journal limit change applies only when the share is bound again", "share", s.ID)
		}

		return nil
	}

	j, err := e.onlyJournal()
	if err != nil {
		e.mu.Unlock()

		return err
	}

	_, raised := e.raised[s.Shard]
	e.busy[s.ID] = struct{}{}
	e.mu.Unlock()

	defer e.endCall(s.ID)

	nb, err := e.attach(ctx, s, j, raised)
	if err != nil {
		return err
	}

	e.publish(nb)

	return nil
}

// validShare refuses a share Bind cannot attach: a zero ID or tag, a
// negative limit or a state the engine does not know.
func validShare(s Share) error {
	var why string

	switch {
	case s.ID.IsZero():
		why = "the share ID is zero"
	case s.Tag == 0:
		why = "the tag is zero"
	case s.Limit < 0:
		why = "the journal limit is negative"
	case s.State != Serving && s.State != Quiesced:
		why = "the state is unknown"
	default:
		return nil
	}

	return fmt.Errorf("engine: share %v: %s: %w", s.ID, why, vfserr.ErrInvalid)
}

// change applies s to the served share's binding: its state alone. A share's
// shard is drawn once and never reused (RFC 16 §2.3.1), and its tag is never
// given twice (RFC 16 §4.2), so a Bind naming others is refused.
func (b *binding) change(s Share) error {
	var why string

	switch {
	case s.Tag != b.share.Tag:
		why = "a served share's tag never changes"
	case s.Shard != b.share.Shard:
		why = "a served share's shard never changes"
	default:
		b.state.Store(uint32(s.State))

		return nil
	}

	return fmt.Errorf("engine: share %v: %s: %w", s.ID, why, vfserr.ErrInvalid)
}

// onlyJournal is the journal a share attaches to: the one open. The caller
// holds mu. With more than one open, the share's journal is not known, as
// nothing the engine reads names it yet.
func (e *Engine) onlyJournal() (Journal, error) {
	switch len(e.journals) {
	case 0:
		return nil, errNotStarted
	case 1:
		return e.journals[0], nil
	}

	return nil, fmt.Errorf("engine: more than one journal open, and nothing names the share's: %w", errUnbuilt)
}

// endCall ends the lifecycle call on share id.
func (e *Engine) endCall(id entity.ShareID) {
	e.mu.Lock()
	delete(e.busy, id)
	e.mu.Unlock()
}

// attach opens the share's handle on j above its floor, writes the share's
// limit at the journal's and raises the shard's incarnation, in that order
// (RFC 8 §2.5). A failure closes the handle and publishes nothing; the limit
// and the incarnation, once raised, stay raised, as neither is ever lowered.
//
// decision: a share with no record in the store is bound like any other, not
// refused. A share has no record only once its removal has finished (RFC 16
// §2.3.2), its ShareID and tag never reused, and nothing deletes a share
// record before that step, so no live share reaches Bind without one; the
// filesystem's admission refuses a share with no record before any operation
// reaches the engine. Should such a share reach Bind, the cost is small: the
// start raise reads its deleted shard record as zero and writes it again, an
// orphan shard key that collides with nothing, as shard numbers are never
// reused (RFC 16 §2.3.1). Overturned by a path that deletes a share record
// while the share is still served.
func (e *Engine) attach(ctx context.Context, s Share, j Journal, raised bool) (*binding, error) {
	ids := []entity.ShareID{s.ID}

	floor, err := e.d.Content.VersionFloor(ctx, ids)
	if err != nil {
		return nil, storeErr(err)
	}

	limits, err := e.d.Lifecycle.VersionLimits(ctx, ids)
	if err != nil {
		return nil, storeErr(err)
	}

	floor = entity.MaxVersion(floor, limits[s.ID])

	h, err := j.Share(s.Tag, s.Limit, journal.VersionFromBytes(floor))
	if err != nil {
		return nil, e.journalErr(err)
	}

	inc, err := e.raise(ctx, s, j, raised)
	if err != nil {
		if cerr := h.Close(); cerr != nil {
			return nil, errors.Join(err, e.journalErr(cerr))
		}

		return nil, err
	}

	b := &binding{
		share:       s,
		j:           j,
		h:           h,
		incarnation: inc,
		gc:          e.groupCommitFor(j),
	}
	b.state.Store(uint32(s.State))

	return b, nil
}

// raise writes the share's limit at the journal's, as a tracked max-write,
// then raises the shard's incarnation: keeping its grace instance when this
// process has raised the shard already, and opening a new one otherwise
// (RFC 16 §3). It returns the incarnation it wrote.
func (e *Engine) raise(ctx context.Context, s Share, j Journal, raised bool) (uint64, error) {
	limit := j.Stats().VersionLimit
	to := map[entity.ShareID]entity.JournalVersion{
		s.ID: entity.JournalVersion(limit.Bytes()),
	}

	if err := e.d.Lifecycle.RaiseVersionLimits(ctx, to); err != nil {
		return 0, storeErr(err)
	}

	if raised {
		inc, err := e.d.Lifecycle.RaiseAtAttach(ctx, s.Shard)
		if err != nil {
			return 0, storeErr(err)
		}

		return inc, nil
	}

	incs, err := e.d.Lifecycle.RaiseAtStart(ctx, []entity.ShardID{s.Shard})
	if err != nil {
		return 0, storeErr(err)
	}

	return incs[s.Shard], nil
}

// publish puts b in a copy of the share table and swaps it in, and records
// its shard as raised.
func (e *Engine) publish(b *binding) {
	e.mu.Lock()

	defer e.mu.Unlock()

	t := make(bindings, len(e.table())+1)
	maps.Copy(t, e.table())
	t[b.share.ID] = b
	e.shares.Store(&t)
	e.raised[b.share.Shard] = struct{}{}
}

// storeErr is Bind's answer for an error the store returned: the error of
// the call's own context as itself, and any other as ErrUnavailable, so no
// store type crosses the engine's boundary.
func storeErr(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}

	return fmt.Errorf("engine: store: %v: %w", err, vfserr.ErrUnavailable)
}
