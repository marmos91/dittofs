package openstate

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
)

// GraceRef names one grace instance of one shard.
type GraceRef struct {
	Shard    ShardID
	Instance uint64
}

// Recovery is the view of the shards this table serves, their grace after a
// loss, and a client's references with no file handle; it runs at the
// primary of the shard each names.
type Recovery interface {
	// ServeShard begins serving s under its grace instance: a call minting
	// state in a shard not begun is refused. Unless s is already served
	// under instance, its state was lost, so s enters grace for one lease
	// period: only the clients its records name may reclaim, and every
	// other request for state is ErrGrace. Grace ends early once every such
	// client has finished reclaiming or has ended. The restart-notice number
	// is raised before grace when an NLM host is named, and each client
	// told by Revoked is told then.
	ServeShard(ctx context.Context, s ShardID, instance uint64) error
	// ReclaimComplete ends the caller's reclaim in the grace instances it
	// was shown, and in no later one of the same shard.
	ReclaimComplete(ctx context.Context, who Caller, shown []GraceRef) error
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

// served is a shard this table serves.
type served struct {
	instance uint64        // the grace instance its references are minted under
	ends     time.Duration // the Elapsed reading its grace ends at; zero: not in grace
	// reclaim is the clients its records named when grace began, by how each
	// recovers: those that may reclaim here and have not finished.
	reclaim map[entity.ClientID]entity.RecoveryKind
	// guarded is set when one of them can hold a deny mode or a mandatory
	// lock: anonymous I/O then waits for grace's end.
	guarded bool
}

// inGrace reports whether sh is in grace at now.
func (sh *served) inGrace(now time.Duration) bool { return sh.ends != 0 && now < sh.ends }

// settle ends sh's grace early once no client it waits for is left; the
// caller holds cmu.
//
// decision: a client that recovers by reconnecting counts as finished at
// once, because the table installs no persistent open for it to reconnect,
// and its volatile state is never reclaimed. Count it until it has
// reconnected each persistent open, or each Timeout has passed, once
// persistent opens are installed at ServeShard.
func (sh *served) settle() {
	for _, k := range sh.reclaim {
		if k != entity.RecoverReconnect {
			return
		}
	}
	sh.ends = 0
}

func (v recovery) ServeShard(ctx context.Context, s ShardID, instance uint64) error {
	t := v.t
	sink, err := t.running()
	if err != nil {
		return err
	}
	t.cmu.Lock()
	cur := t.shards[s]
	t.cmu.Unlock()
	if cur != nil && cur.instance == instance {
		return nil // a handover of the table: nothing was lost
	}
	sh := &served{instance: instance, reclaim: map[entity.ClientID]entity.RecoveryKind{}}
	var told []Revocation
	notice := false
	for rec, err := range t.d.Records.Clients().ByShard(ctx, s) {
		if err != nil {
			return err
		}
		sh.reclaim[rec.ID] = rec.Recovery
		sh.guarded = sh.guarded || rec.Protocol != entity.ProtocolNLM
		if rec.Recovery == entity.RecoverReconnect || rec.Recovery == entity.RecoverNotice {
			told = append(told, Revocation{Client: rec.ID, Shard: s, Reason: RevokedShardState, Adapter: rec.Adapter})
			notice = notice || rec.Recovery == entity.RecoverNotice
		}
	}
	if notice {
		if err := t.raiseNotice(ctx); err != nil {
			return err
		}
	}
	t.cmu.Lock()
	sh.ends = t.d.Clock.Elapsed() + leasePeriod
	sh.settle()
	t.shards[s] = sh
	t.cmu.Unlock()
	for _, r := range told {
		sink.Revoked(r)
	}
	return nil
}

// raiseNotice raises the restart-notice number durably, so every NLM host
// sees the loss.
func (t *Table) raiseNotice(ctx context.Context) error {
	for {
		n, err := t.d.Records.NSMState(ctx)
		if err != nil {
			return err
		}
		if err = t.d.Records.RaiseNSMState(ctx, n+1); !errors.Is(err, kvstore.ErrNSMNotRaised) {
			return err
		}
	}
}

// admit admits a request of who for state in shard s, a reclaim or not, and
// reports whether s is in grace. A shard not begun is ErrDelay; a client
// that owes a reclaim it must connect again for is ErrStaleClient; in grace
// a request that is no reclaim is ErrGrace, and a reclaim by a client the
// shard's records did not name, or that finished, ErrNoReclaim.
//
// decision: a shard not begun answers ErrDelay, a retry, since the service
// begins every shard it serves before it answers, so a request there races a
// takeover. Answer the error a moved shard's request gets once shards move.
func (t *Table) admit(who Caller, s ShardID, reclaim bool) (bool, error) {
	t.cmu.Lock()
	defer t.cmu.Unlock()
	sh := t.shards[s]
	if sh == nil {
		return false, ErrDelay
	}
	if t.mustConnectLocked(who.Client) {
		return false, ErrStaleClient
	}
	grace := sh.inGrace(t.d.Clock.Elapsed())
	_, may := sh.reclaim[who.Client]
	switch {
	case grace && !reclaim:
		return true, ErrGrace
	case grace && !may:
		return true, ErrNoReclaim
	}
	return grace, nil
}

// guarded reports whether anonymous I/O in shard s waits for grace's end: s
// is in grace, and its records named a client that can hold a deny mode or a
// mandatory lock, which a reclaim may still restore.
//
// decision: anonymous I/O names no state, so the shard is the one the
// service resolved for the file, carried in the reference's State.Shard. Take
// a FileKey in CheckIO if the service is found unable to fill it.
func (t *Table) guarded(s ShardID) bool {
	t.cmu.Lock()
	defer t.cmu.Unlock()
	sh := t.shards[s]
	return sh != nil && sh.guarded && sh.inGrace(t.d.Clock.Elapsed())
}

// owedLocked is the grace instances client c has not finished, by the
// marks the table holds of its record; the caller holds cmu.
func (t *Table) owedLocked(c entity.ClientID) []GraceRef {
	var owed []GraceRef
	for _, m := range t.known[c].shards {
		if sh := t.shards[m.Shard]; sh != nil && m.Done < sh.instance {
			owed = append(owed, GraceRef{m.Shard, sh.instance})
		}
	}
	return owed
}

// mustConnect is mustConnectLocked, taking cmu.
func (t *Table) mustConnect(c entity.ClientID) bool {
	t.cmu.Lock()
	defer t.cmu.Unlock()
	return t.mustConnectLocked(c)
}

// mustConnectLocked reports whether c recovers by connecting again and owes
// a reclaim: every call naming it is then ErrStaleClient until it does. The
// caller holds cmu.
func (t *Table) mustConnectLocked(c entity.ClientID) bool {
	return t.known[c].recovery == entity.RecoverRefusal && len(t.owedLocked(c)) > 0
}

// unreclaim drops c from every grace waiting on it: it ended.
func (t *Table) unreclaim(c entity.ClientID) {
	t.cmu.Lock()
	defer t.cmu.Unlock()
	for _, sh := range t.shards {
		if _, ok := sh.reclaim[c]; ok {
			delete(sh.reclaim, c)
			sh.settle()
		}
	}
}

// decision: a pair whose shard this table does not serve, or whose instance
// is not above the client's Done or is above the shard's own, is skipped: the
// client was shown nothing it may end there. Answer it ErrInvalid if an
// adapter is found naming pairs it was never shown.
func (v recovery) ReclaimComplete(ctx context.Context, who Caller, shown []GraceRef) error {
	t := v.t
	if _, err := t.running(); err != nil {
		return err
	}
	t.cstep.Lock()
	defer t.cstep.Unlock()
	rec, err := t.d.Records.Clients().Get(ctx, who.Client)
	switch {
	case err != nil:
		return staleIfGone(err)
	case rec.Protect && who.Principal != rec.Owner:
		return ErrNotYours
	}
	for _, g := range shown {
		i := slices.IndexFunc(rec.Shards, func(m entity.ShardMark) bool { return m.Shard == g.Shard })
		t.cmu.Lock()
		sh := t.shards[g.Shard]
		t.cmu.Unlock()
		if i < 0 || sh == nil || g.Instance <= rec.Shards[i].Done || g.Instance > sh.instance {
			continue
		}
		m := entity.ShardMark{Shard: g.Shard, Done: g.Instance}
		if err := t.d.Records.Clients().MarkShard(ctx, who.Client, m); err != nil {
			return staleIfGone(err)
		}
		t.cmu.Lock()
		t.noteMarkLocked(who.Client, m)
		if sh.instance == g.Instance {
			delete(sh.reclaim, who.Client)
			sh.settle()
		}
		t.cmu.Unlock()
	}
	return nil
}

// noteMarkLocked records m in what the table holds of c's record; the caller
// holds cmu.
func (t *Table) noteMarkLocked(c entity.ClientID, m entity.ShardMark) {
	k, ok := t.known[c]
	if !ok {
		return
	}
	k.shards = slices.Clone(k.shards)
	if i := slices.IndexFunc(k.shards, func(x entity.ShardMark) bool { return x.Shard == m.Shard }); i >= 0 {
		k.shards[i] = m
	} else {
		k.shards = append(k.shards, m)
	}
	t.known[c] = k
}

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
