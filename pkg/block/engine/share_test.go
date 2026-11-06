package engine_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// errInjected is the failure a fault view answers.
var errInjected = errors.New("injected store failure")

// errClose is the failure a recorded handle's Close answers.
var errClose = errors.New("injected close failure")

// faultContent fails VersionFloor with err.
type faultContent struct {
	engine.Content
	err error
}

func (c faultContent) VersionFloor(context.Context, []entity.ShareID) (entity.JournalVersion, error) {
	return entity.JournalVersion{}, c.err
}

// gate holds a call: it closes entered once the call arrives, then waits for
// release.
type gate struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGate(t *testing.T) *gate {
	g := &gate{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}

	t.Cleanup(g.open)

	return g
}

// open releases the held call, once.
func (g *gate) open() {
	g.once.Do(func() {
		close(g.release)
	})
}

// faultLifecycle fails VersionLimits or the start raise with the error set,
// and holds the next start raise at the gate armed, once.
type faultLifecycle struct {
	engine.Lifecycle
	limitsErr error
	raiseErr  error
	gate      atomic.Pointer[gate]
}

func (l *faultLifecycle) VersionLimits(
	ctx context.Context,
	shares []entity.ShareID,
) (map[entity.ShareID]entity.JournalVersion, error) {
	if l.limitsErr != nil {
		return nil, l.limitsErr
	}

	return l.Lifecycle.VersionLimits(ctx, shares)
}

func (l *faultLifecycle) RaiseAtStart(
	ctx context.Context,
	shards []entity.ShardID,
) (map[entity.ShardID]uint64, error) {
	if g := l.gate.Swap(nil); g != nil {
		close(g.entered)
		<-g.release
	}

	if l.raiseErr != nil {
		return nil, l.raiseErr
	}

	return l.Lifecycle.RaiseAtStart(ctx, shards)
}

// logRecorder keeps every record logged through it.
type logRecorder struct {
	mu   *sync.Mutex
	recs *[]slog.Record
}

func newLogRecorder() logRecorder {
	return logRecorder{
		mu:   &sync.Mutex{},
		recs: &[]slog.Record{},
	}
}

func (l logRecorder) Enabled(context.Context, slog.Level) bool {
	return true
}

func (l logRecorder) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	*l.recs = append(*l.recs, r.Clone())
	l.mu.Unlock()

	return nil
}

func (l logRecorder) WithAttrs([]slog.Attr) slog.Handler {
	return l
}

func (l logRecorder) WithGroup(string) slog.Handler {
	return l
}

// warnings is every record logged at Warn.
func (l logRecorder) warnings() []slog.Record {
	l.mu.Lock()

	defer l.mu.Unlock()

	var out []slog.Record

	for _, r := range *l.recs {
		if r.Level == slog.LevelWarn {
			out = append(out, r)
		}
	}

	return out
}

// isDelay reports whether err is ErrDelay with cause c.
func isDelay(err error, c vfserr.DelayCause) bool {
	var d *vfserr.Delay

	return errors.As(err, &d) && d.Cause == c
}

// commitAt commits existence for file id of the unit's share at v, so the
// share's version floor is at least v.
func (u *unit) commitAt(t *testing.T, id entity.FileID, v entity.JournalVersion) {
	t.Helper()

	now := time.Now()
	pending := []kvstore.PendingExistence{
		{
			Share: u.share.ID,
			File:  id,
			Ops: []kvstore.PendingOp{
				{
					Version: v,
					Kind:    kvstore.OpWrite,
					Extent:  entity.Extent{Length: 4096},
					Mtime:   now,
					Ctime:   now,
				},
			},
		},
	}

	failed, err := u.fs.Store.Existence().Commit(t.Context(), pending, nil)
	if err != nil || failed[id] != nil {
		t.Fatalf("commit existence at %v: %v, %v", v, err, failed[id])
	}
}

// Bind passes the journal's Share the larger of the share's version floor
// and its version limit, and the journal's counter moves above it.
func TestBindPassesTheLargerOfFloorAndLimit(t *testing.T) {
	for _, c := range []struct {
		name               string
		existence, limit   uint64 // offsets above the store's floor
		wantExistenceFloor bool
	}{
		{"the floor is larger", 2e12, 1e12, true},
		{"the limit is larger", 1e12, 2e12, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			u := newUnitWith(t, unitConfig{unbound: true})
			ctx := t.Context()
			_, file := u.fs.Create(t, u.fs.Admin, u.fs.Root, "f", kvstore.Regular(0o644))

			base, err := u.fs.Store.Content().VersionFloor(ctx, []entity.ShareID{u.share.ID})
			if err != nil {
				t.Fatal(err)
			}

			ex := entity.VersionOf(base.Counter() + c.existence)
			lim := entity.VersionOf(base.Counter() + c.limit)
			u.commitAt(t, file.ID, ex)

			to := map[entity.ShareID]entity.JournalVersion{
				u.share.ID: lim,
			}

			if err := u.fs.Store.Lifecycle().RaiseVersionLimits(ctx, to); err != nil {
				t.Fatal(err)
			}

			// The commit stamps the file at least at ex: the store's floor
			// is read back, not assumed.
			floor, err := u.fs.Store.Content().VersionFloor(ctx, []entity.ShareID{u.share.ID})
			if err != nil {
				t.Fatal(err)
			}

			want := lim

			if c.wantExistenceFloor {
				want = floor
			}

			if floor.Less(ex) || want.Less(floor) || want.Less(lim) {
				t.Fatalf("setup: floor %d, limit %d; want the %s", floor.Counter(), lim.Counter(), c.name)
			}

			if err := u.e.Bind(ctx, u.share); err != nil {
				t.Fatal(err)
			}

			if got := u.rec.floor(0); got.Counter != want.Counter() {
				t.Errorf("Share got floor %d, want the larger, %d", got.Counter, want.Counter())
			}

			d, err := u.e.Draw(ctx, u.share.ID, entity.NewFileID())
			if err != nil {
				t.Fatal(err)
			}

			if d.Counter() <= want.Counter() {
				t.Errorf("the first version drawn is %d, want above %d", d.Counter(), want.Counter())
			}
		})
	}
}

// Bind writes the share's version limit at the journal's.
func TestBindWritesTheShareLimitAtTheJournals(t *testing.T) {
	u := newUnit(t)

	got, err := u.fs.Store.Lifecycle().VersionLimits(t.Context(), []entity.ShareID{u.share.ID})
	if err != nil {
		t.Fatal(err)
	}

	if got[u.share.ID] != entity.VersionOf(unitLimit) {
		t.Errorf("the share's limit is %d, want the journal's %d", got[u.share.ID].Counter(), uint64(unitLimit))
	}
}

// A Bind that fails at any step publishes nothing, leaves no handle open and
// answers a vfserr value: the store's errors as ErrUnavailable, and the
// context's error as itself.
func TestAFailedBindLeavesNothing(t *testing.T) {
	for _, c := range []struct {
		name     string
		content  error // VersionFloor's failure
		limits   error // VersionLimits' failure
		raise    error // the start raise's failure
		closed   bool  // the journal is closed, so Share fails
		closeErr error // the handle's Close fails too
		want     error
	}{
		{name: "the floor read", content: errInjected, want: vfserr.ErrUnavailable},
		{name: "the limit read", limits: errInjected, want: vfserr.ErrUnavailable},
		{name: "the journal's Share", closed: true, want: vfserr.ErrUnavailable},
		{name: "the start raise", raise: errInjected, want: vfserr.ErrUnavailable},
		{name: "the context", raise: context.Canceled, want: context.Canceled},
		{
			name:     "the start raise, then the handle's Close",
			raise:    errInjected,
			closeErr: errClose,
			want:     vfserr.ErrUnavailable,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			fl := &faultLifecycle{
				limitsErr: c.limits,
				raiseErr:  c.raise,
			}
			faults := func(d *engine.Deps) {
				fl.Lifecycle = d.Lifecycle
				d.Lifecycle = fl

				if c.content != nil {
					d.Content = faultContent{
						Content: d.Content,
						err:     c.content,
					}
				}
			}
			u := newUnitWith(t, unitConfig{
				deps:    []func(*engine.Deps){faults},
				unbound: true,
			})

			s := u.share
			u.rec.closeErr = c.closeErr

			if c.closed {
				if err := u.j.Close(); err != nil {
					t.Fatal(err)
				}
			}

			before := u.e.TableForTest()
			err := u.e.Bind(t.Context(), s)
			if !errors.Is(err, c.want) {
				t.Errorf("Bind = %v, want %v", err, c.want)
			}

			if n := u.rec.open(); n != 0 {
				t.Errorf("%d handles left open", n)
			}

			// A Close that fails as well is reported beside the failure
			// that undid the attach, both answered as ErrUnavailable. The
			// journal's error crosses as text only, as journalErr maps it.
			if c.closeErr != nil {
				var joined interface {
					Unwrap() []error
				}

				ok := errors.As(err, &joined) && len(joined.Unwrap()) == 2

				if !ok || !strings.Contains(joined.Unwrap()[1].Error(), c.closeErr.Error()) {
					t.Errorf("Bind = %v, want the raise's failure and the Close's", err)
				} else if !errors.Is(joined.Unwrap()[1], vfserr.ErrUnavailable) {
					t.Errorf("the Close's failure %v is not ErrUnavailable", joined.Unwrap()[1])
				}
			}

			if err := u.e.BindingOf(s.ID); !isDelay(err, vfserr.DelayFrozen) {
				t.Errorf("after the failed Bind the share is bound: %v", err)
			}

			if u.e.TableForTest() != before {
				t.Error("a failed Bind swapped the share table")
			}
		})
	}
}

// A share with no record in the store, one whose removal has finished, is
// bound like any other: Bind answers nil and binds it, opens its handle, and
// writes no version limit for it.
func TestBindOfAShareWithNoRecordBindsIt(t *testing.T) {
	u := newUnitWith(t, unitConfig{unbound: true})
	ctx := t.Context()
	s := u.share
	s.ID = entity.NewShareID()

	if err := u.e.Bind(ctx, s); err != nil {
		t.Fatalf("Bind of a share with no record = %v, want it bound", err)
	}

	if err := u.e.BindingOf(s.ID); err != nil {
		t.Errorf("the share is not bound: %v", err)
	}

	if n := u.rec.shares(); n != 1 {
		t.Errorf("the journal saw %d Share calls, want 1", n)
	}

	limits, err := u.fs.Store.Lifecycle().VersionLimits(ctx, []entity.ShareID{s.ID})
	if err != nil {
		t.Fatal(err)
	}

	if !limits[s.ID].IsZero() {
		t.Errorf("Bind wrote version limit %d for a share with no record", limits[s.ID].Counter())
	}
}

// An invalid share is refused before any I/O: a share not served is left
// unbound with no journal or store call, and a served one keeps the binding
// it had.
func TestBindRefusesInvalidSettingsBeforeAnyIO(t *testing.T) {
	u := newUnit(t)
	b := enginetest.AddShare(t, u.fs.Store, "b", 0)

	for _, c := range []struct {
		name string
		s    engine.Share
	}{
		{"a negative limit", invalid(b, -1, b.State, b.ID, b.Tag)},
		{"an unknown state", invalid(b, b.Limit, 9, b.ID, b.Tag)},
		{"a zero ID", invalid(b, b.Limit, b.State, entity.ShareID{}, b.Tag)},
		{"a zero tag", invalid(b, b.Limit, b.State, b.ID, 0)},
	} {
		t.Run("not served, "+c.name, func(t *testing.T) {
			s := c.s
			shares := u.rec.shares()
			u.fs.Counts.Take()

			err := u.e.Bind(t.Context(), s)
			if !errors.Is(err, vfserr.ErrInvalid) {
				t.Errorf("Bind = %v, want ErrInvalid", err)
			}

			if n := u.rec.shares(); n != shares {
				t.Errorf("the journal saw %d Share calls", n-shares)
			}

			if got := u.fs.Counts.Take(); got != (kv.Counts{}) {
				t.Errorf("the store saw %+v", got)
			}

			if err := u.e.BindingOf(s.ID); !isDelay(err, vfserr.DelayFrozen) {
				t.Errorf("the refused share is bound: %v", err)
			}
		})
	}

	a := u.share

	for _, c := range []struct {
		name string
		s    engine.Share
	}{
		{"a negative limit", invalid(a, -1, engine.Quiesced, a.ID, a.Tag)},
		{"an unknown state", invalid(a, a.Limit, 9, a.ID, a.Tag)},
	} {
		t.Run("served, "+c.name, func(t *testing.T) {
			s := c.s

			if err := u.e.Bind(t.Context(), s); !errors.Is(err, vfserr.ErrInvalid) {
				t.Errorf("Bind = %v, want ErrInvalid", err)
			}

			if got := u.e.StateOf(u.share.ID); got != engine.Serving {
				t.Errorf("the served share's state is %v, want it kept serving", got)
			}

			if _, err := u.e.Draw(t.Context(), u.share.ID, entity.NewFileID()); err != nil {
				t.Errorf("the served share no longer draws: %v", err)
			}
		})
	}
}

// invalid is s with the fields a validation row sets.
func invalid(
	s engine.Share,
	limit int64,
	state engine.ShareState,
	id entity.ShareID,
	tag journal.ShareTag,
) engine.Share {
	s.Limit = limit
	s.State = state
	s.ID = id
	s.Tag = tag

	return s
}

// A Bind of a served share that changes its tag or shard is
// refused, and the binding is kept as it was.
func TestBindOfAServedShareRefusesAnIdentityChange(t *testing.T) {
	u := newUnit(t)
	a := u.share
	tag := a
	tag.Tag++
	shard := a
	shard.Shard++

	for _, c := range []struct {
		name string
		s    engine.Share
	}{
		{"the tag", tag},
		{"the shard", shard},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := c.s
			s.State = engine.Quiesced

			if err := u.e.Bind(t.Context(), s); !errors.Is(err, vfserr.ErrInvalid) {
				t.Errorf("Bind = %v, want ErrInvalid", err)
			}

			if got := u.e.StateOf(u.share.ID); got != engine.Serving {
				t.Errorf("the share's state is %v, want it kept serving", got)
			}

			if n := u.rec.shares(); n != 1 {
				t.Errorf("the journal saw %d Share calls, want the first alone", n)
			}
		})
	}
}

// A Bind of a served share with another journal limit applies its state
// change, keeps the limit the handle uses and warns, on every such Bind,
// until the share is bound again.
func TestBindOfAServedShareKeepsItsLimitAndWarns(t *testing.T) {
	logs := newLogRecorder()
	u := newUnit(t, func(d *engine.Deps) {
		d.Logger = slog.New(logs)
	})

	for _, c := range []struct {
		limit int64
		state engine.ShareState
		warns int
	}{
		{2 << 30, engine.Quiesced, 1},
		{2 << 30, engine.Serving, 2},
		{1 << 30, engine.Serving, 2},
	} {
		s := u.share
		s.Limit = c.limit
		s.State = c.state

		if err := u.e.Bind(t.Context(), s); err != nil {
			t.Fatalf("Bind with limit %d: %v", c.limit, err)
		}

		if got := u.e.StateOf(s.ID); got != c.state {
			t.Errorf("limit %d: the state is %v, want %v", c.limit, got, c.state)
		}

		if n := len(logs.warnings()); n != c.warns {
			t.Errorf("limit %d: %d warnings, want %d", c.limit, n, c.warns)
		}
	}

	if n := u.rec.shares(); n != 1 {
		t.Errorf("the journal saw %d Share calls, want the first alone", n)
	}

	for _, r := range logs.warnings() {
		if strings.Contains(r.Message, "1073741824") || strings.Contains(r.Message, "2147483648") {
			t.Errorf("the warning %q carries a limit", r.Message)
		}

		share := false

		r.Attrs(func(a slog.Attr) bool {
			share = share || a.Key == "share"

			return true
		})

		if !share {
			t.Errorf("the warning %q names no share", r.Message)
		}
	}
}

// A state change swaps nothing: the table stays the same, the share's state
// follows each call, and another share's writes are served throughout.
func TestAStateChangeSwapsNothing(t *testing.T) {
	u := newUnit(t)
	b := enginetest.AddShare(t, u.fs.Store, "b", 0)

	if err := u.e.Bind(t.Context(), b); err != nil {
		t.Fatal(err)
	}

	table := u.e.TableForTest()

	for _, state := range []engine.ShareState{engine.Quiesced, engine.Serving} {
		s := u.share
		s.State = state

		if err := u.e.Bind(t.Context(), s); err != nil {
			t.Fatal(err)
		}

		if got := u.e.StateOf(s.ID); got != state {
			t.Errorf("the state is %v, want %v", got, state)
		}

		f := entity.NewFileID()
		_, err := u.e.Write(t.Context(), b.ID, f, 0, []byte("b"), engine.WriteOpts{Done: func() {}})
		if err != nil || len(u.e.Pending(f)) != 1 {
			t.Errorf("the other share's write: %v, %d pending", err, len(u.e.Pending(f)))
		}
	}

	if u.e.TableForTest() != table {
		t.Error("a state change swapped the share table")
	}
}

// shardOf reads shard id's record back from the store.
func (u *unit) shardOf(t *testing.T, id entity.ShardID) entity.Shard {
	t.Helper()

	sh, err := u.fs.Store.Shard(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}

	return sh
}

// The first Bind of a shard this process has not raised opens a new grace
// instance: GraceInstance equals the new incarnation.
func TestAFirstBindAfterARestartTakesTheStartRaise(t *testing.T) {
	u := newUnitWith(t, unitConfig{unbound: true})
	before := u.shardOf(t, u.share.Shard)

	if err := u.e.Bind(t.Context(), u.share); err != nil {
		t.Fatal(err)
	}

	after := u.shardOf(t, u.share.Shard)

	if after.Incarnation != before.Incarnation+1 || after.GraceInstance != after.Incarnation {
		t.Errorf("shard %+v after the Bind of %+v; want the incarnation up one and the grace instance on it", after, before)
	}
}

// A Bind of a shard this process has raised keeps its grace instance.
func TestABindOfARaisedShardKeepsItsGrace(t *testing.T) {
	u := newUnit(t)
	b := enginetest.AddShare(t, u.fs.Store, "b", 0)
	u.e.MarkRaisedForTest(b.Shard)
	before := u.shardOf(t, b.Shard)

	if err := u.e.Bind(t.Context(), b); err != nil {
		t.Fatal(err)
	}

	after := u.shardOf(t, b.Shard)

	if after.Incarnation != before.Incarnation+1 || after.GraceInstance != before.GraceInstance {
		t.Errorf("shard %+v after the Bind of %+v; want the incarnation up one and the grace instance kept", after, before)
	}
}

// Bind attaches to the one journal open: with none it is unavailable and
// reads no store; with two it is refused as not built, and neither journal
// sees a Share.
func TestBindNeedsExactlyOneJournal(t *testing.T) {
	t.Run("none", func(t *testing.T) {
		u := newUnitWith(t, unitConfig{unbound: true})
		u.e.SetJournalsForTest()
		u.fs.Counts.Take()

		err := u.e.Bind(t.Context(), u.share)
		if !errors.Is(err, engine.ErrNotStarted) || !errors.Is(err, vfserr.ErrUnavailable) {
			t.Errorf("Bind with no journal = %v, want the not-started ErrUnavailable", err)
		}

		if got := u.fs.Counts.Take(); got != (kv.Counts{}) {
			t.Errorf("the store saw %+v", got)
		}
	})

	t.Run("two", func(t *testing.T) {
		u := newUnitWith(t, unitConfig{unbound: true})
		other := &recJournal{
			FakeJournal: enginetest.NewFakeJournal(),
		}
		u.e.SetJournalsForTest(u.rec, other)

		err := u.e.Bind(t.Context(), u.share)
		if !errors.Is(err, vfserr.ErrUnavailable) || !strings.Contains(err.Error(), "more than one journal open") {
			t.Errorf("Bind with two journals = %v, want the unbuilt refusal", err)
		}

		if u.rec.shares()+other.shares() != 0 {
			t.Errorf("Share calls: %d and %d, want none", u.rec.shares(), other.shares())
		}

		if err := u.e.BindingOf(u.share.ID); !isDelay(err, vfserr.DelayFrozen) {
			t.Errorf("the share is bound: %v", err)
		}
	})
}

// While a share's Bind waits on the store, the lifecycle mutex is free, a
// second Bind of the share answers ErrDelay with DelayConflict, and another
// share's Bind completes.
func TestASecondLifecycleCallOnTheShareIsDelayed(t *testing.T) {
	fl := &faultLifecycle{}
	u := newUnit(t, func(d *engine.Deps) {
		fl.Lifecycle = d.Lifecycle
		d.Lifecycle = fl
	})
	b := enginetest.AddShare(t, u.fs.Store, "b", 0)
	c := enginetest.AddShare(t, u.fs.Store, "c", 0)

	g := newGate(t)
	fl.gate.Store(g)
	done := make(chan error, 1)
	finished := make(chan struct{})

	go func() {
		defer close(finished)

		done <- u.e.Bind(context.Background(), b)
	}()

	// Registered after the gate's, so it runs first: it releases the held
	// Bind and waits for it, so no goroutine outlives the test.
	t.Cleanup(func() {
		g.open()
		<-finished
	})

	select {
	case <-g.entered:
	case err := <-done:
		t.Fatalf("the Bind ended (%v) without reaching the start raise", err)
	}

	if !u.e.LifecycleFree() {
		t.Fatal("the lifecycle mutex is held while a Bind waits on the store")
	}

	if err := u.e.Bind(t.Context(), b); !isDelay(err, vfserr.DelayConflict) {
		t.Errorf("a second Bind of the share = %v, want ErrDelay(DelayConflict)", err)
	}

	if n := u.rec.shares(); n != 2 {
		t.Errorf("the journal saw %d Share calls, want 2", n)
	}

	if err := u.e.Bind(t.Context(), c); err != nil {
		t.Errorf("another share's Bind = %v, want it bound", err)
	}

	g.open()

	if err := <-done; err != nil {
		t.Fatalf("the held Bind = %v", err)
	}

	b.State = engine.Quiesced

	if err := u.e.Bind(t.Context(), b); err != nil {
		t.Errorf("a Bind after the first ended = %v", err)
	}
}

// A content call answers while the lifecycle mutex is held. A content call
// that took the mutex would hang here, until the test's timeout.
func TestAContentCallIgnoresTheLifecycleMutex(t *testing.T) {
	u := newUnit(t)
	release := u.e.HoldLifecycle()

	_, err := u.e.Draw(t.Context(), u.share.ID, entity.NewFileID())
	release()

	if err != nil {
		t.Errorf("Draw with the lifecycle mutex held = %v", err)
	}
}

// Readers find their share through every Bind of 64 others, each of which
// copies the table.
func TestTheTableUnderReadersAndBinds(t *testing.T) {
	u := newUnitWith(t, unitConfig{unbound: true})

	if err := u.e.Bind(t.Context(), u.share); err != nil {
		t.Fatal(err)
	}

	others := make([]engine.Share, 64)

	for i := range others {
		others[i] = enginetest.AddShare(t, u.fs.Store, "s"+string(rune('a'+i%26))+string(rune('a'+i/26)), 0)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(4)

	for range 4 {
		go func() {
			defer wg.Done()

			for {
				select {
				case <-stop:
					return
				default:
				}

				if _, err := u.e.Draw(context.Background(), u.share.ID, entity.NewFileID()); err != nil {
					t.Errorf("a reader lost the share: %v", err)

					return
				}
			}
		}()
	}

	for _, s := range others {
		if err := u.e.Bind(t.Context(), s); err != nil {
			t.Error(err)
		}
	}

	close(stop)
	wg.Wait()

	for _, s := range append(others, u.share) {
		if err := u.e.BindingOf(s.ID); err != nil {
			t.Errorf("share %v is not in the table: %v", s.ID, err)
		}
	}
}
