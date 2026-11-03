package quota

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// store is Store over maps: limits, their crossing times and usage per
// target, and the SoftExceeded calls made.
type store struct {
	mu     sync.Mutex
	limits map[Target]entity.Quota
	since  map[Target]time.Time
	used   map[Target]entity.Usage
	soft   []bool
}

func newStore() *store {
	return &store{limits: map[Target]entity.Quota{}, since: map[Target]time.Time{}, used: map[Target]entity.Usage{}}
}

func (s *store) Quota(_ context.Context, _ entity.ShareID, t Target) (entity.Quota, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.limits[t], s.since[t], nil
}

func (s *store) Usage(_ context.Context, _ entity.ShareID, t Target) (entity.Usage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.used[t], nil
}

func (s *store) SoftExceeded(_ context.Context, _ entity.ShareID, t Target, above bool) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.soft = append(s.soft, above)
	switch {
	case !above:
		delete(s.since, t)
	case s.since[t].IsZero():
		s.since[t] = time.Now()
	}
	return s.since[t], nil
}

// events records what is emitted.
type events []vfs.QuotaEvent

func (e *events) emit(_ context.Context, ev vfs.QuotaEvent) { *e = append(*e, ev) }

func (e events) kinds() []vfs.QuotaEventKind {
	var out []vfs.QuotaEventKind
	for _, ev := range e {
		out = append(out, ev.Kind)
	}
	return out
}

var (
	share       = entity.NewShareID()
	alice       = entity.Principal{Kind: entity.PrincipalUser, ID: entity.PrincipalID{1}}
	staff       = entity.Principal{Kind: entity.PrincipalGroup, ID: entity.PrincipalID{2}}
	charge      = Charge{Share: share, Owner: alice, Group: staff, Project: 7}
	hardBytes   = func(n int64) entity.Quota { return entity.Quota{Hard: entity.Limit{Bytes: n}} }
	scopeTarget = map[vfs.QuotaScope]Target{
		vfs.QuotaShare: {}, vfs.QuotaProject: {Project: 7}, vfs.QuotaUser: {Principal: alice}, vfs.QuotaGroup: {Principal: staff},
	}
)

func scopeOf(err error) vfs.QuotaScope {
	var qe *vfs.QuotaError
	if errors.As(err, &qe) {
		return qe.Scope
	}
	return 0
}

// A reservation past several quotas names the first in the order share,
// project, user, group; each refuses alone; and a refusal emits a hard
// refusal naming the quota and the writer.
func TestTheFirstScopeExceededRefuses(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	var ev events
	q := New(s, ev.emit)
	writer := vfs.Identity{Proof: vfs.ProofTicket, Client: netip.MustParseAddr("10.0.0.5")}
	for _, sc := range []vfs.QuotaScope{vfs.QuotaGroup, vfs.QuotaUser, vfs.QuotaProject, vfs.QuotaShare} {
		s.limits[scopeTarget[sc]] = hardBytes(10)
		ev = nil
		_, err := q.Reserve(ctx, writer, charge, entity.Usage{Bytes: 11})
		if got := scopeOf(err); got != sc || !errors.Is(err, vfs.ErrQuota) {
			t.Errorf("past every quota up to %v: %v, want it named", sc, err)
		}
		if len(ev) != 1 || ev[0].Kind != vfs.QuotaHardRefused || ev[0].Scope != sc || ev[0].Subject != scopeTarget[sc].Principal ||
			ev[0].Project != scopeTarget[sc].Project || ev[0].Writer.Client != writer.Client || ev[0].Hard != 10 || ev[0].Resource != vfs.QuotaBytes {
			t.Errorf("refused by %v: emitted %+v", sc, ev)
		}
	}
	if r, err := q.Reserve(ctx, writer, charge, entity.Usage{Bytes: 10}); err != nil || r == nil {
		t.Errorf("a reservation up to every limit: %v", err)
	}
}

// Reservations held count against a limit with the usage committed until
// each is released, once however often its release is called; a zero one
// holds nothing.
func TestAReservationIsHeldUntilReleased(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	q := New(s, (&events{}).emit)
	s.limits[Target{Principal: alice}] = entity.Quota{Hard: entity.Limit{Bytes: 10, Files: 1}}
	s.used[Target{Principal: alice}] = entity.Usage{Bytes: 2}
	first, err := q.Reserve(ctx, vfs.Identity{}, charge, entity.Usage{Bytes: 5})
	if err != nil {
		t.Fatal(err)
	}
	second, err := q.Reserve(ctx, vfs.Identity{}, charge, entity.Usage{Bytes: 3})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.Reserve(ctx, vfs.Identity{}, charge, entity.Usage{Bytes: 1}); scopeOf(err) != vfs.QuotaUser {
		t.Errorf("past the limit with 8 bytes reserved over 2 used: %v, want refused", err)
	}
	first.Release()
	first.Release()
	if r, err := q.Reserve(ctx, vfs.Identity{}, charge, entity.Usage{Bytes: 5}); err != nil {
		t.Errorf("5 bytes with 3 reserved over 2 used: %v", err)
	} else {
		r.Release()
	}
	if _, err := q.Reserve(ctx, vfs.Identity{}, charge, entity.Usage{Bytes: 6}); scopeOf(err) != vfs.QuotaUser {
		t.Errorf("a second release of one reservation freed another's: %v", err)
	}
	second.Release()
	if r, err := q.Reserve(ctx, vfs.Identity{}, charge, entity.Usage{}); r != nil || err != nil {
		t.Errorf("a zero reservation: %v, %v", r, err)
	}
	if _, err := q.Reserve(ctx, vfs.Identity{}, charge, entity.Usage{Files: 2}); scopeOf(err) != vfs.QuotaUser {
		t.Errorf("two inodes past a limit of one: %v", err)
	}
}

// The soft limit refuses nothing while its grace runs, and refuses as the
// hard one once it has run, the expiry emitted once and forgotten once usage
// is back under.
func TestTheSoftLimitIsHardOnceItsGraceHasRun(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	var ev events
	q := New(s, ev.emit)
	ut := Target{Principal: alice}
	s.limits[ut] = entity.Quota{Soft: entity.Limit{Bytes: 10}, Hard: entity.Limit{Bytes: 100}, Grace: time.Hour}
	s.used[ut] = entity.Usage{Bytes: 20}
	s.since[ut] = time.Now().Add(-time.Minute)
	if r, err := q.Reserve(ctx, vfs.Identity{}, charge, entity.Usage{Bytes: 1}); err != nil {
		t.Errorf("over the soft limit within its grace: %v", err)
	} else {
		r.Release()
	}
	s.since[ut] = time.Now().Add(-2 * time.Hour)
	for range 2 {
		if _, err := q.Reserve(ctx, vfs.Identity{}, charge, entity.Usage{Bytes: 1}); scopeOf(err) != vfs.QuotaUser {
			t.Errorf("over the soft limit past its grace: %v, want refused", err)
		}
	}
	want := []vfs.QuotaEventKind{vfs.QuotaGraceExpired, vfs.QuotaHardRefused, vfs.QuotaHardRefused}
	if got := ev.kinds(); len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("emitted %v, want %v", got, want)
	}

	// Back under, the expiry emitted for that crossing is forgotten.
	s.used[ut] = entity.Usage{}
	if r, err := q.Reserve(ctx, vfs.Identity{}, charge, entity.Usage{Bytes: 1}); err != nil {
		t.Fatal(err)
	} else {
		r.Release()
	}
	if len(q.expired) != 0 {
		t.Errorf("back under, the expiries kept: %v", q.expired)
	}
}

// An admitted reservation emits the advisory and soft crossings it makes,
// once each, recording the soft one; one finding usage back under a soft
// limit recorded as passed clears the record and emits it.
func TestCrossingsAreEmittedOnce(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	var ev events
	q := New(s, ev.emit)
	ut := Target{Principal: alice}
	s.limits[ut] = entity.Quota{Advisory: entity.Limit{Bytes: 5}, Soft: entity.Limit{Bytes: 8}, Grace: time.Hour}
	reserve := func(n int64) {
		t.Helper()
		r, err := q.Reserve(ctx, vfs.Identity{}, charge, entity.Usage{Bytes: n})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(r.Release)
	}
	reserve(6) // 6: past the advisory threshold
	reserve(1) // 7: nothing new
	reserve(2) // 9: past the soft limit
	reserve(1) // 10: nothing new
	if s.since[ut].IsZero() || len(s.soft) != 1 {
		t.Errorf("the soft crossing is recorded %v by %v", s.since[ut], s.soft)
	}
	want := []vfs.QuotaEventKind{vfs.QuotaAdvisoryCrossed, vfs.QuotaSoftExceeded}
	if got := ev.kinds(); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("emitted %v, want %v", got, want)
	}
	if ev[1].GraceEnd != s.since[ut].Add(time.Hour) || ev[1].Used != 9 || ev[1].Soft != 8 {
		t.Errorf("the soft crossing: %+v", ev[1])
	}

	// The reservations released and the usage committed lower, the next
	// check finds it back under.
	q2 := New(s, ev.emit)
	ev = nil
	if r, err := q2.Reserve(ctx, vfs.Identity{}, charge, entity.Usage{Bytes: 1}); err != nil {
		t.Fatal(err)
	} else {
		r.Release()
	}
	if !s.since[ut].IsZero() || len(ev) != 1 || ev[0].Kind != vfs.QuotaBackUnder {
		t.Errorf("back under: recorded since %v, emitted %+v", s.since[ut], ev)
	}
}

// barrier is a store whose every Usage read waits until n have been made,
// so n reservations all judge the same record before any decides.
type barrier struct {
	*store
	wg *sync.WaitGroup
}

func (b barrier) Usage(ctx context.Context, share entity.ShareID, t Target) (entity.Usage, error) {
	b.wg.Done()
	b.wg.Wait()
	return b.store.Usage(ctx, share, t)
}

// Concurrent reservations that all read the soft limit unrecorded, and later
// all read it recorded, emit its crossing and its going back under once each.
func TestConcurrentCrossingsAreEmittedOnce(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	ut := Target{Principal: alice}
	s.limits[ut] = entity.Quota{Soft: entity.Limit{Bytes: 8}, Grace: time.Hour}
	var mu sync.Mutex
	var ev events
	emit := func(ctx context.Context, e vfs.QuotaEvent) {
		mu.Lock()
		defer mu.Unlock()
		ev.emit(ctx, e)
	}
	const n = 8
	run := func(each int64) {
		t.Helper()
		var wg sync.WaitGroup
		wg.Add(n)
		q := New(barrier{s, &wg}, emit)
		var done sync.WaitGroup
		rs := make([]*Reservation, n)
		for i := range n {
			done.Go(func() {
				var err error
				if rs[i], err = q.Reserve(ctx, vfs.Identity{}, charge, entity.Usage{Bytes: each}); err != nil {
					t.Error(err)
				}
			})
		}
		done.Wait()
		for _, r := range rs {
			r.Release()
		}
	}
	run(2) // 16 reserved in all: past the soft limit of 8
	if got := ev.kinds(); len(got) != 1 || got[0] != vfs.QuotaSoftExceeded {
		t.Errorf("crossing concurrently: emitted %v, want one soft crossing", got)
	}
	ev = nil
	s.limits[ut] = entity.Quota{Soft: entity.Limit{Bytes: 100}, Grace: time.Hour}
	run(1)
	if got := ev.kinds(); len(got) != 1 || got[0] != vfs.QuotaBackUnder {
		t.Errorf("going back under concurrently: emitted %v, want one", got)
	}
}

// A file whose group is its owner, a group of one, is held once against the
// owner's quota, not once as owner and again as group.
func TestAGroupOfOneIsHeldOnce(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	q := New(s, (&events{}).emit)
	s.limits[Target{Principal: alice}] = hardBytes(10)
	own := Charge{Share: share, Owner: alice, Group: alice}
	if _, err := q.Reserve(ctx, vfs.Identity{}, own, entity.Usage{Bytes: 6}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Reserve(ctx, vfs.Identity{}, own, entity.Usage{Bytes: 4}); err != nil {
		t.Errorf("4 bytes with 6 held under a limit of 10: %v", err)
	}
}

// A Rehome refused against the new owner moves nothing, and one admitted
// and then reverted moves the file's write reservations back, so either
// way they count against the old owner, not the new. One admitted and kept
// moves them, each still returned by its write's Release.
func TestARehomeMovesTheFilesWritesOnlyOnceAdmittedAndKept(t *testing.T) {
	ctx := context.Background()
	bob := entity.Principal{Kind: entity.PrincipalUser, ID: entity.PrincipalID{3}}
	f := charge
	f.File = entity.NewFileID()
	to, his := f, Charge{Share: share, Owner: bob}
	to.Owner = bob
	inode := entity.Usage{Files: 1}
	setup := func(bobs int64) (*store, *Quotas, *Reservation) {
		s := newStore()
		s.limits[Target{Principal: alice}] = hardBytes(15)
		s.limits[Target{Principal: bob}] = hardBytes(bobs)
		q := New(s, (&events{}).emit)
		w, err := q.Reserve(ctx, vfs.Identity{}, f, entity.Usage{Bytes: 10})
		if err != nil {
			t.Fatal(err)
		}
		return s, q, w
	}

	_, q, _ := setup(9)
	if _, err := q.Rehome(ctx, vfs.Identity{}, to, inode, vfs.QuotaUser); scopeOf(err) != vfs.QuotaUser {
		t.Fatalf("a rehome of a 10-byte write to bob, at a limit of 9: %v, want QuotaUser", err)
	}
	if _, err := q.Reserve(ctx, vfs.Identity{}, his, entity.Usage{Bytes: 9}); err != nil {
		t.Errorf("9 bytes for bob after a refused rehome: %v, want them admitted, nothing moved to him", err)
	}
	if _, err := q.Reserve(ctx, vfs.Identity{}, charge, entity.Usage{Bytes: 6}); scopeOf(err) != vfs.QuotaUser {
		t.Errorf("6 bytes for alice, a 10-byte write held of her 15, after a refused rehome: %v, want QuotaUser", err)
	}

	_, q, w := setup(10)
	moved, err := q.Rehome(ctx, vfs.Identity{}, to, inode, vfs.QuotaUser)
	if err != nil {
		t.Fatal(err)
	}
	moved.Revert()
	if _, err := q.Reserve(ctx, vfs.Identity{}, his, entity.Usage{Bytes: 10}); err != nil {
		t.Errorf("10 bytes for bob, at a limit of 10, after a reverted rehome: %v, want them admitted", err)
	}
	if _, err := q.Reserve(ctx, vfs.Identity{}, charge, entity.Usage{Bytes: 6}); scopeOf(err) != vfs.QuotaUser {
		t.Errorf("6 bytes for alice, a 10-byte write moved back of her 15, after a reverted rehome: %v, want QuotaUser", err)
	}
	w.Release()
	if _, err := q.Reserve(ctx, vfs.Identity{}, charge, entity.Usage{Bytes: 15}); err != nil {
		t.Errorf("15 bytes for alice once the write moved back is released: %v, want them admitted", err)
	}

	_, q, w = setup(10)
	if moved, err = q.Rehome(ctx, vfs.Identity{}, to, inode, vfs.QuotaUser); err != nil {
		t.Fatal(err)
	}
	moved.Release()
	if _, err := q.Reserve(ctx, vfs.Identity{}, his, entity.Usage{Bytes: 1}); scopeOf(err) != vfs.QuotaUser {
		t.Errorf("a byte for bob, a moved 10-byte write held of his 10: %v, want QuotaUser", err)
	}
	if _, err := q.Reserve(ctx, vfs.Identity{}, charge, entity.Usage{Bytes: 15}); err != nil {
		t.Errorf("15 bytes for alice, her write moved to bob: %v, want them admitted", err)
	}
	w.Release()
	if _, err := q.Reserve(ctx, vfs.Identity{}, his, entity.Usage{Bytes: 10}); err != nil {
		t.Errorf("10 bytes for bob once the moved write is released: %v, want them admitted", err)
	}

	// A write released between the move and the revert stays released: the
	// revert moves back only what is still outstanding.
	_, q, w = setup(10)
	if moved, err = q.Rehome(ctx, vfs.Identity{}, to, inode, vfs.QuotaUser); err != nil {
		t.Fatal(err)
	}
	w.Release()
	moved.Revert()
	if _, err := q.Reserve(ctx, vfs.Identity{}, charge, entity.Usage{Bytes: 15}); err != nil {
		t.Errorf("15 bytes for alice, her moved write released before the revert: %v, want them admitted", err)
	}
}

// releasing is a store whose Usage read of target waits until done has run
// to its end, as a store read waits on a commit whose Done runs meanwhile.
type releasing struct {
	*store
	target Target
	done   func()
}

func (r releasing) Usage(ctx context.Context, share entity.ShareID, t Target) (entity.Usage, error) {
	if t == r.target {
		over := make(chan struct{})
		go func() { r.done(); close(over) }()
		<-over
	}
	return r.store.Usage(ctx, share, t)
}

// A Rehome reads the store before the tracker's mutex, so a Release of
// another file's reservation, which takes that mutex, runs to its end while
// the Rehome's store read waits on it.
func TestARehomeHoldsNoMutexAcrossItsStoreRead(t *testing.T) {
	ctx := context.Background()
	bob := entity.Principal{Kind: entity.PrincipalUser, ID: entity.PrincipalID{3}}
	s := newStore()
	s.limits[Target{Principal: bob}] = hardBytes(10)
	var other *Reservation
	q := New(releasing{s, Target{Principal: bob}, func() { other.Release() }}, (&events{}).emit)
	elsewhere := charge
	elsewhere.File = entity.NewFileID()
	var err error
	if other, err = q.Reserve(ctx, vfs.Identity{}, elsewhere, entity.Usage{Bytes: 5}); err != nil {
		t.Fatal(err)
	}
	to := charge
	to.Owner, to.File = bob, entity.NewFileID()
	rehomed := make(chan error, 1)
	go func() {
		_, err := q.Rehome(ctx, vfs.Identity{}, to, entity.Usage{Files: 1}, vfs.QuotaUser)
		rehomed <- err
	}()
	select {
	case err := <-rehomed:
		if err != nil {
			t.Errorf("a rehome to bob: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("a rehome whose store read waits on another reservation's Release did not return within 1s")
	}
}
