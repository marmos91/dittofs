package openstate

import (
	"context"
	"slices"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
)

// WatchID names a watch by its shard, grace instance bits and token, as a
// StateRef does, so Unwatch resolves it from the ID alone.
type WatchID struct {
	Shard    ShardID
	Instance uint8
	Token    uint64
}

func (w WatchID) ref() StateRef {
	return StateRef{Type: StateWatch, Shard: w.Shard, Instance: w.Instance, Token: w.Token}
}

// Change is one change a watch reports.
type Change struct {
	What   ChangeMask // the kind of change, matched against a watch's filter; ChangeEnumerate: changes were dropped, and the client lists the directory again
	Name   []byte     // the entry changed, as its path from the directory Changed names
	To     []byte     // a rename's new path; nil otherwise
	Nested bool       // made below the directory Changed names, not in it: only a recursive watch reports it
}

// Watches is the view of change notifications on directories; it runs at the
// directory's primary, and Unwatch at the primary of the shard the WatchID
// names.
type Watches interface {
	// Watch registers who's watch of the changes to dir that m names, and
	// with recursive of those below it too. It is volatile and never
	// reclaimed: in grace it is ErrGrace, and it ends with the client's
	// lease, a revoke or Unwatch. A client past its watches here is ErrDelay.
	Watch(ctx context.Context, who Caller, dir FileKey, recursive bool, m ChangeMask) (WatchID, error)
	// Unwatch ends who's watch w, dropping the changes it has not reported.
	Unwatch(ctx context.Context, who Caller, w WatchID) error
	// Changed reports a change the service committed in dir, or below it
	// with ch.Nested, to each watch of dir it matches, through Sink.Notify.
	// It never fails the change. A watch reports its first change at once
	// and holds the changes after it until the timer hands them over, at
	// most 64 or 64 KiB of them: past either they collapse to one
	// ChangeEnumerate. So it hands the Sink at most 65 changes a sweep, and
	// what the client has not read is the Sink's to bound.
	Changed(ctx context.Context, dir FileKey, ch Change)
}

type watches struct{ t *Table }

// Watches is the view of change notifications.
func (t *Table) Watches() Watches { return watches{t} }

// ponytail: a watch holds at most 64 changes or 64 KiB of them, and hands
// what it holds to the Sink at once only when it handed nothing since the
// timer last looked; the rest wait for the timer, so a busy directory's
// changes reach the client up to a sweep late. Hand them over on the
// client's read instead, if a call is added that tells the table when the
// client reads.
const (
	maxChanges     = 64
	maxChangeBytes = 64 << 10
)

// watch is a watch as the table holds it, on its directory's entry.
type watch struct {
	id        WatchID
	client    entity.ClientID
	recursive bool
	filter    ChangeMask
	pending   []Change // matched and not yet handed to the Sink
	size      int      // the bytes pending holds
	handed    bool     // changes went to the Sink since the timer last looked
}

// noteOut is one Notify.
type noteOut struct {
	c  entity.ClientID
	w  WatchID
	ch []Change
}

// changeSize is what ch costs a watch holding it.
func changeSize(ch Change) int { return 16 + len(ch.Name) + len(ch.To) }

func (v watches) Watch(ctx context.Context, who Caller, dir FileKey, recursive bool, m ChangeMask) (WatchID, error) {
	t := v.t
	if _, err := t.running(); err != nil {
		return WatchID{}, err
	}
	var id WatchID
	err := t.step(dir.ID, func(e *entry) error {
		if _, err := t.admit(who, dir.Shard, false); err != nil {
			return err
		}
		ref, err := t.mintUnder(e, who.Client, dir.Shard, StateWatch, func(StateRef) error {
			if !t.current(who.Client) {
				return ErrStaleClient
			}
			return t.chargeLocked(who.Client, cappedWatches, 1, true)
		})
		if err != nil {
			return err
		}
		id = WatchID{ref.Shard, ref.Instance, ref.Token}
		if e.watches == nil {
			e.watches = map[uint64]*watch{}
		}
		e.watches[ref.Token] = &watch{id: id, client: who.Client, recursive: recursive, filter: m}
		return nil
	})
	return id, err
}

func (v watches) Unwatch(ctx context.Context, who Caller, id WatchID) error {
	t := v.t
	if _, err := t.running(); err != nil {
		return err
	}
	h, err := t.resolve(who.Client, entity.FileID{}, id.ref())
	if err != nil {
		return err
	}
	return t.step(h.file, func(e *entry) error {
		if _, err := t.resolve(who.Client, e.file, id.ref()); err != nil {
			return err
		}
		t.dropWatch(e, e.watches[id.Token])
		return nil
	})
}

// dropWatch drops w from e, under its step, with what it holds.
func (t *Table) dropWatch(e *entry, w *watch) {
	delete(e.watches, w.id.Token)
	t.release(e, w.id.ref())
	_ = t.charge(w.client, cappedWatches, -1, false)
}

func (v watches) Changed(ctx context.Context, dir FileKey, ch Change) {
	t := v.t
	sink, err := t.running()
	if err != nil {
		return
	}
	// A directory no one watches has no entry: it is not made for a change.
	fs := &t.files[dir.ID[0]]
	fs.mu.Lock()
	_, held := fs.entries[dir.ID]
	fs.mu.Unlock()
	if !held {
		return
	}
	ch.Name, ch.To = slices.Clone(ch.Name), slices.Clone(ch.To)
	var out outbox
	_ = t.step(dir.ID, func(e *entry) error {
		for _, w := range e.watches {
			if w.filter&ch.What == 0 || ch.Nested && !w.recursive {
				continue
			}
			w.hold(ch)
			if !w.handed {
				w.hand(&out)
			}
			t.idx.Lock()
			t.watched[e.file] = struct{}{}
			t.idx.Unlock()
		}
		return nil
	})
	out.raise(t, sink)
}

// hold adds ch to w's pending changes; past their bound they collapse to one
// ChangeEnumerate, and stay one.
//
// decision: this bound, with the per-client watch cap, replaces the SMB
// adapter's changenotify.NotifyRegistry overflow, which caps watches at
// MaxPendingWatches for the whole node, once the adapter moves onto the
// table; until then the registry is the live one. The overflow past a
// request's MaxOutputLength stays in the adapter, which alone sees the
// request: what it holds for an unread client must still collapse to
// STATUS_NOTIFY_ENUM_DIR when it does not fit (MS-FSA 2.1.5.10).
func (w *watch) hold(ch Change) {
	switch {
	case len(w.pending) == 1 && w.pending[0].What == ChangeEnumerate:
	case len(w.pending) >= maxChanges || w.size+changeSize(ch) > maxChangeBytes:
		w.pending, w.size = []Change{{What: ChangeEnumerate}}, 0
	default:
		w.pending, w.size = append(w.pending, ch), w.size+changeSize(ch)
	}
}

// hand moves w's pending changes to out.
func (w *watch) hand(out *outbox) {
	out.notes = append(out.notes, noteOut{w.client, w.id, w.pending})
	w.pending, w.size, w.handed = nil, 0, true
}

// handWatched hands each watch on dirs what it holds, for the timer, and
// lets one that held nothing hand its next change at once; a directory
// leaves the timer's set once none of its watches handed anything.
func (t *Table) handWatched(sink Sink, dirs []entity.FileID) {
	for _, d := range dirs {
		var out outbox
		_ = t.step(d, func(e *entry) error {
			busy := false
			for _, w := range e.watches {
				if w.handed = len(w.pending) > 0; w.handed {
					w.hand(&out)
				}
				busy = busy || w.handed
			}
			if !busy {
				t.idx.Lock()
				delete(t.watched, d)
				t.idx.Unlock()
			}
			return nil
		})
		out.raise(t, sink)
	}
}
