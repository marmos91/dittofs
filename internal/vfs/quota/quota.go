// Package quota is the filesystem's one enforcer of quotas. A write that
// grows a file, a create, and an owner or group change, reserves what it
// adds before anything is staged; the reservation is judged against every quota covering it, the
// share's, the project's, the owner's and the group's, in that order, and
// held in memory until the commit that charges it lands or the write is
// dropped. It reads the store only through Store, and emits each quota event
// where it decides.
package quota

import (
	"context"
	"sync"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// Target is what one quota limits in a share: a user or group, a project,
// or, with neither set, the share itself.
type Target struct {
	Principal entity.Principal
	Project   uint64
}

// Store is what quota reads: a target's limits and when its usage first went
// above its soft limit, zero when it is not above it; its committed usage,
// unfolded changes included; and the one write, SoftExceeded, which records
// whether usage is above the soft limit and answers since when.
type Store interface {
	Quota(ctx context.Context, share entity.ShareID, t Target) (q entity.Quota, since time.Time, err error)
	Usage(ctx context.Context, share entity.ShareID, t Target) (entity.Usage, error)
	SoftExceeded(ctx context.Context, share entity.ShareID, t Target, above bool) (since time.Time, err error)
}

// Charge is whom a reservation charges: the file's owner and group, or for a
// create the new file's, and the project it is in.
type Charge struct {
	Share        entity.ShareID
	Owner, Group entity.Principal
	Project      uint64
	// Moved is a charge moving between principals, an owner or group
	// change: the share's quota is not judged, as the charge leaves neither
	// it nor the project, which a moved charge leaves zero.
	Moved bool
}

// Quotas holds every reservation made on this node and not yet released.
type Quotas struct {
	store Store
	emit  func(context.Context, vfs.QuotaEvent)

	mu      sync.Mutex
	held    map[slot]entity.Usage
	expired map[slot]time.Time // the soft-limit crossing whose grace expiry has been emitted
	// above is whether this node last found each quota above its soft
	// limit; a quota it has not judged is as the store records it.
	//
	// ponytail: an entry stays once a quota has crossed its soft limit, so
	// the map holds one per quota ever crossed on this node. Drop entries
	// with no reservation held once the count of quotas makes that show.
	above map[slot]bool
}

type slot struct {
	share entity.ShareID
	t     Target
}

// New is quota over s, emitting each event to emit, which never blocks.
func New(s Store, emit func(context.Context, vfs.QuotaEvent)) *Quotas {
	return &Quotas{store: s, emit: emit, held: map[slot]entity.Usage{}, expired: map[slot]time.Time{}, above: map[slot]bool{}}
}

// Reservation is one write's or create's reservation. Release returns it,
// once whatever how many times it is called; a nil one holds nothing.
type Reservation struct {
	q     *Quotas
	slots []slot
	add   entity.Usage
	once  sync.Once
}

// Release returns the reservation.
func (r *Reservation) Release() {
	if r == nil {
		return
	}
	r.once.Do(func() {
		r.q.mu.Lock()
		defer r.q.mu.Unlock()
		for _, s := range r.slots {
			if u := minus(r.q.held[s], r.add); u == (entity.Usage{}) {
				delete(r.q.held, s)
			} else {
				r.q.held[s] = u
			}
		}
	})
}

// judged is one quota covering a reservation, as read before it is decided.
type judged struct {
	scope vfs.QuotaScope
	slot  slot
	lim   entity.Quota
	since time.Time
	used  entity.Usage
}

// Reserve reserves add, charged to c, for writer, judging it against every
// quota covering c in the scopes' order: share, project, user, group. A
// quota refuses when what is committed, plus what this node holds reserved,
// plus add, passes its hard limit, or its soft limit once its grace has run
// from when usage first went above it; the first refusing scope is answered
// as a *vfs.QuotaError, and a hard refusal emitted. An admitted reservation
// emits the advisory and soft-limit crossings it makes and, when it finds
// usage back under a soft limit recorded as exceeded, clears the record. A
// zero add reserves nothing and answers a nil reservation.
//
// Usage is read before the reservations are, so a commit landing between
// the two, its reservation released, is counted by neither: a hard limit is
// passed by at most what commits concurrently add.
//
// decision: a principal with no quota of its own has no limit, as the store
// keeps no default per kind for it to fall back on. Read the share's default
// for the principal's kind once the store keeps one.
//
// ponytail: no reservation is refused for slack, as every primary of a share
// is this one node, where slack bounds nothing. Refuse ErrDelay (DelayCommit)
// past each primary's slack once a quota's writers can span primaries.
//
// ponytail: each reservation reads every covering quota's record and, for
// each that limits, the share's usage, on the write path. Cache the limits
// per share, dropped when one is set, when a write profile shows the reads.
func (q *Quotas) Reserve(ctx context.Context, writer vfs.Identity, c Charge, add entity.Usage) (*Reservation, error) {
	if add == (entity.Usage{}) {
		return nil, nil
	}
	scopes := []struct {
		scope vfs.QuotaScope
		t     Target
		ok    bool
	}{
		{vfs.QuotaShare, Target{}, !c.Moved},
		{vfs.QuotaProject, Target{Project: c.Project}, c.Project != 0},
		{vfs.QuotaUser, Target{Principal: c.Owner}, !c.Owner.IsZero()},
		{vfs.QuotaGroup, Target{Principal: c.Group}, !c.Group.IsZero() && c.Group != c.Owner},
	}
	r := &Reservation{q: q, add: add}
	var js []judged
	for _, s := range scopes {
		if !s.ok {
			continue
		}
		sl := slot{c.Share, s.t}
		r.slots = append(r.slots, sl)
		lim, since, err := q.store.Quota(ctx, c.Share, s.t)
		if err != nil {
			return nil, err
		}
		if lim.Unlimited() {
			continue
		}
		used, err := q.store.Usage(ctx, c.Share, s.t)
		if err != nil {
			return nil, err
		}
		js = append(js, judged{s.scope, sl, lim, since, used})
	}
	now := time.Now()
	before, expired, flipped, refused, res := q.decide(js, r, now)
	for _, i := range expired {
		q.event(ctx, js[i], vfs.QuotaGraceExpired, overSoft(js[i].lim, before[i]), before[i], now, vfs.Identity{})
	}
	if refused >= 0 {
		j := js[refused]
		q.event(ctx, j, vfs.QuotaHardRefused, res, before[refused], now, writer)
		return nil, &vfs.QuotaError{Scope: j.scope}
	}
	for i, j := range js {
		after := plus(before[i], add)
		if res, over := passes(after, j.lim.Advisory); over {
			if _, was := passes(before[i], j.lim.Advisory); !was {
				q.event(ctx, j, vfs.QuotaAdvisoryCrossed, res, after, now, vfs.Identity{})
			}
		}
		if !flipped[i] {
			continue
		}
		res, above := passes(after, j.lim.Soft)
		// A record that fails to be written is forgotten here, so the next
		// check judges against the store's record and writes it; the
		// reservation stands either way.
		since, err := q.store.SoftExceeded(ctx, c.Share, j.slot.t, above)
		if err != nil {
			q.mu.Lock()
			delete(q.above, j.slot)
			q.mu.Unlock()
			continue
		}
		if above {
			j.since = since
			q.event(ctx, j, vfs.QuotaSoftExceeded, res, after, now, vfs.Identity{})
		} else {
			q.event(ctx, j, vfs.QuotaBackUnder, overSoft(j.lim, before[i]), after, now, vfs.Identity{})
		}
	}
	return r, nil
}

// decide judges js, in order, under the lock every reservation is made
// under, and adds r to what is held unless one refuses: before is each
// quota's usage with what this node holds reserved, without r; expired the
// quotas whose grace has run, each named once per crossing; refused the first
// that refuses r, -1 for none, and res the resource it refuses on. Admitted,
// flipped marks the quotas r takes across their soft limit, either way, each
// crossing claimed by one reservation alone; one going back under forgets
// its grace expiry.
func (q *Quotas) decide(js []judged, r *Reservation, now time.Time) (before []entity.Usage, expired []int, flipped []bool, refused int, res vfs.QuotaResource) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, j := range js {
		b := plus(j.used, q.held[j.slot])
		before = append(before, b)
		if graceRun(j, now) && q.expired[j.slot] != j.since {
			q.expired[j.slot] = j.since
			expired = append(expired, i)
		}
		if res, over := passes(plus(b, r.add), hard(j, now)); over {
			return before, expired, nil, i, res
		}
	}
	for _, s := range r.slots {
		q.held[s] = plus(q.held[s], r.add)
	}
	flipped = make([]bool, len(js))
	for i, j := range js {
		_, above := passes(plus(before[i], r.add), j.lim.Soft)
		was, ok := q.above[j.slot]
		if !ok {
			was = !j.since.IsZero()
		}
		if above == was {
			continue
		}
		q.above[j.slot], flipped[i] = above, true
		if !above {
			delete(q.expired, j.slot)
		}
	}
	return before, expired, flipped, -1, 0
}

// graceRun reports whether j's soft limit is enforced as its hard one: usage
// went above it, and the grace has run since.
func graceRun(j judged, now time.Time) bool {
	return !j.since.IsZero() && !now.Before(j.since.Add(j.lim.Grace))
}

// hard is the limit j refuses past: its hard limit, and, once its grace has
// run, its soft limit where that is tighter.
func hard(j judged, now time.Time) entity.Limit {
	l := j.lim.Hard
	if graceRun(j, now) {
		l.Bytes, l.Files = tighter(l.Bytes, j.lim.Soft.Bytes), tighter(l.Files, j.lim.Soft.Files)
	}
	return l
}

// tighter is the lower of two limits, where zero limits nothing.
func tighter(a, b int64) int64 {
	if a == 0 || b != 0 && b < a {
		return b
	}
	return a
}

// passes reports whether u passes l in either count, and which; a zero count
// limits nothing. Bytes are judged first.
func passes(u entity.Usage, l entity.Limit) (vfs.QuotaResource, bool) {
	switch {
	case l.Bytes > 0 && u.Bytes > l.Bytes:
		return vfs.QuotaBytes, true
	case l.Files > 0 && u.Files > l.Files:
		return vfs.QuotaFiles, true
	}
	return 0, false
}

// overSoft is the resource a soft limit counts that u is judged in: the one
// it passes, else bytes when it limits bytes, else files.
func overSoft(l entity.Quota, u entity.Usage) vfs.QuotaResource {
	if r, over := passes(u, l.Soft); over {
		return r
	}
	if l.Soft.Bytes > 0 {
		return vfs.QuotaBytes
	}
	return vfs.QuotaFiles
}

// event emits one event on j's quota, judged in resource r at usage u.
func (q *Quotas) event(ctx context.Context, j judged, kind vfs.QuotaEventKind, r vfs.QuotaResource, u entity.Usage, now time.Time, writer vfs.Identity) {
	e := vfs.QuotaEvent{
		Time: now, Share: j.slot.share, Scope: j.scope, Subject: j.slot.t.Principal, Project: j.slot.t.Project,
		Resource: r, Kind: kind, Used: u.Bytes, Soft: j.lim.Soft.Bytes, Hard: j.lim.Hard.Bytes, Writer: writer,
	}
	if r == vfs.QuotaFiles {
		e.Used, e.Soft, e.Hard = u.Files, j.lim.Soft.Files, j.lim.Hard.Files
	}
	if !j.since.IsZero() {
		e.GraceEnd = j.since.Add(j.lim.Grace)
	}
	q.emit(ctx, e)
}

func plus(a, b entity.Usage) entity.Usage {
	return entity.Usage{Bytes: a.Bytes + b.Bytes, Files: a.Files + b.Files}
}

func minus(a, b entity.Usage) entity.Usage {
	return entity.Usage{Bytes: a.Bytes - b.Bytes, Files: a.Files - b.Files}
}
