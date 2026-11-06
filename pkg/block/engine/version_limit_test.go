package engine_test

import (
	"context"
	"errors"
	"log/slog"
	"runtime"
	"slices"
	"strings"
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

// testBlock is the version block of the raise tests: half of it, 2048, is
// above the rate term of every rate they hold steady.
const testBlock = 4096

// drawFile is the file the raise tests draw versions for: Draw takes no
// lock and creates no entry, so any file does.
var drawFile = entity.FileID{0xd7}

// newRaiser is the started engine of a raise test: the starter's, with a
// block of testBlock, its raises tracked for WaitRaises, and an existence age
// of age, or no settings at all when age is zero.
func newRaiser(t *testing.T, age time.Duration, opts ...starterOption) *starter {
	t.Helper()

	if age == 0 {
		opts = append(opts, noApply())
	}

	s := newStarter(t, opts...)
	s.e.SetBlockForTest(testBlock)
	s.e.TrackRaisesForTest()

	if age != 0 {
		applyAge(t, s, age)
	}

	if err := s.start(t); err != nil {
		t.Fatal(err)
	}

	return s
}

// applyAge applies settings with an existence age of age.
func applyAge(t *testing.T, s *starter, age time.Duration) {
	t.Helper()

	err := s.e.Apply(engine.Settings{
		ExistenceAge:        age,
		GroupCommitMaxFiles: 64,
		CloneMaxLen:         1 << 30,
	})
	if err != nil {
		t.Fatal(err)
	}
}

// drawN draws n versions through the first share, failing on a refusal.
func drawN(t *testing.T, s *starter, n int) {
	t.Helper()

	for range n {
		if _, err := s.e.Draw(t.Context(), s.one.ID, drawFile); err != nil {
			t.Fatalf("draw: %v", err)
		}
	}
}

// waitBound bounds a wait that correct code never reaches: the raise it
// waits for reaches the store at once, and a draw never waits.
const waitBound = 5 * time.Second

// waitForRaise waits for a raise to reach the store gate g, and fails the
// test when none does within waitBound.
func waitForRaise(t *testing.T, g *gate) {
	t.Helper()

	select {
	case <-g.entered:
	case <-time.After(waitBound):
		t.Fatal("no raise reached the store")
	}
}

// drawWhileHeld draws n versions through the first share on a goroutine of
// its own while a raise may be held, at g or waiting on the clock to retry.
// A draw that is still waiting after waitBound fails the test, and g is
// released so a raise held there can end.
func drawWhileHeld(t *testing.T, s *starter, n int, g *gate) {
	t.Helper()

	done := make(chan error, 1)

	go drawAll(s, n, done)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("draw: %v", err)
		}
	case <-time.After(waitBound):
		t.Errorf("a draw waits while a raise is held")
		g.open()
		t.FailNow()
	}
}

// drawAll is the goroutine drawWhileHeld runs: it draws n versions and sends
// the first error, or nil.
func drawAll(s *starter, n int, done chan<- error) {
	for range n {
		if _, err := s.e.Draw(context.Background(), s.one.ID, drawFile); err != nil {
			done <- err

			return
		}
	}

	done <- nil
}

// drawAtRate draws n versions at rate a second: the clock moves a second
// after every rate draws.
func drawAtRate(t *testing.T, s *starter, rate, n int) {
	t.Helper()

	for i := range n {
		drawN(t, s, 1)

		if i%rate == rate-1 {
			s.clock.Advance(time.Second)
		}
	}
}

// left is the versions the fake journal can still issue below its limit.
func left(s *starter) uint64 {
	st := s.j.Stats()

	if st.NextVersion.Counter >= st.VersionLimit.Counter {
		return 0
	}

	return st.VersionLimit.Counter - st.NextVersion.Counter
}

// marks is every limit the engine passed to the journal, the start's first.
func marks(s *starter) []journal.Version {
	s.j.mu.Lock()

	defer s.j.mu.Unlock()

	return slices.Clone(s.j.marks)
}

// limitOf is share id's version limit in the store.
func limitOf(t *testing.T, s *starter, id entity.ShareID) uint64 {
	t.Helper()

	got, err := s.fs.Store.Lifecycle().VersionLimits(t.Context(), []entity.ShareID{id})
	if err != nil {
		t.Fatal(err)
	}

	return got[id].Counter()
}

// setLimit writes share id's version limit in the store at counter.
func setLimit(t *testing.T, s *starter, id entity.ShareID, counter uint64) {
	t.Helper()

	to := map[entity.ShareID]entity.JournalVersion{
		id: entity.VersionOf(counter),
	}

	if err := s.fs.Store.Lifecycle().RaiseVersionLimits(t.Context(), to); err != nil {
		t.Fatal(err)
	}
}

// logsAt is every record logged at level.
func logsAt(s *starter, level slog.Level) []slog.Record {
	s.logs.mu.Lock()

	defer s.logs.mu.Unlock()

	var out []slog.Record

	for _, r := range *s.logs.recs {
		if r.Level == level {
			out = append(out, r)
		}
	}

	return out
}

// bindInBackground binds sh on a goroutine of its own, whose stack names
// bindThird, and returns the channel its error comes on. The test waits for
// that goroutine to end before it does.
func bindInBackground(t *testing.T, s *starter, sh engine.Share) <-chan error {
	done := make(chan error, 1)
	finished := make(chan struct{})

	go bindThird(s, sh, done, finished)

	t.Cleanup(func() {
		<-finished
	})

	return done
}

// bindThird is the goroutine bindInBackground runs.
func bindThird(s *starter, sh engine.Share, done chan<- error, finished chan<- struct{}) {
	defer close(finished)

	done <- s.e.Bind(context.Background(), sh)
}

// waitingInLock reports, once it knows, whether a goroutine waits to take a
// sync.Mutex that caller, a function named as the stack names it, locks.
// It reports false as soon as stop holds. It spins on the scheduler, never
// on the clock, so it ends either way.
func waitingInLock(caller string, stop func() bool) bool {
	buf := make([]byte, 1<<20)

	for {
		if stop() {
			return false
		}

		n := runtime.Stack(buf, true)

		for g := range strings.SplitSeq(string(buf[:n]), "\n\n") {
			if lockedBy(g, caller) {
				return true
			}
		}

		runtime.Gosched()
	}
}

// lockedBy reports whether goroutine stack g waits in sync.(*Mutex).Lock
// called straight from caller.
func lockedBy(g, caller string) bool {
	var funcs []string

	for line := range strings.SplitSeq(g, "\n") {
		if line != "" && !strings.HasPrefix(line, "\t") && !strings.HasPrefix(line, "goroutine ") {
			funcs = append(funcs, line)
		}
	}

	for i, f := range funcs {
		if strings.HasPrefix(f, "sync.(*Mutex).Lock(") && i+1 < len(funcs) {
			return strings.Contains(funcs[i+1], caller)
		}
	}

	return false
}

// waiting reports whether goroutine stack g is in a wait state: anything in
// its header but running or runnable.
func waiting(g string) bool {
	header, _, _ := strings.Cut(g, "\n")
	open := strings.Index(header, "[")
	shut := strings.Index(header, "]")

	if open < 0 || shut < open {
		return false
	}

	state, _, _ := strings.Cut(header[open+1:shut], ",")

	return state != "running" && state != "runnable"
}

// The raise is held in the store; the journal is given no new limit until
// the store has committed it, and every served share holds it after.
func TestRaiseCommitsBeforeTheJournalGetsTheLimit(t *testing.T) {
	s := newRaiser(t, time.Second)
	l0 := marks(s)[0].Counter
	g := newGate(t)
	s.lc.hold.Store(&heldRaise{
		g: g,
	})

	drawWhileHeld(t, s, 125, g)
	waitForRaise(t, g)

	if n := len(marks(s)); n != 1 {
		t.Errorf(
			"the journal was given %d limits while the raise waits on the store, want one",
			n,
		)
	}

	if got := limitOf(t, s, s.one.ID); got != l0 {
		t.Errorf("the store holds %d before the raise commits, want %d", got, l0)
	}

	g.open()
	s.e.WaitRaises(t)

	m := marks(s)

	if len(m) != 2 {
		t.Fatalf("the journal was given %d limits, want 2", len(m))
	}

	for _, id := range []entity.ShareID{s.one.ID, s.two.ID} {
		if got := limitOf(t, s, id); got < m[1].Counter {
			t.Errorf("share %v holds %d below the limit the journal got, %d", id, got, m[1].Counter)
		}
	}
}

// A raise has committed and waits to give the journal its limit; a share
// bound meanwhile waits for it, and holds at least the journal's final
// limit.
func TestBindWaitsForARaiseInFlight(t *testing.T) {
	s := newRaiser(t, time.Second, withSpare())
	third := s.spare
	g := newGate(t)
	s.j.limitGate.Store(g)

	drawWhileHeld(t, s, 125, g)
	waitForRaise(t, g)

	done := bindInBackground(t, s, third)
	// Registered after the Bind's wait, so it runs first and frees the Bind
	// should the test end early.
	t.Cleanup(g.open)
	bound := false

	// A Bind reaches the lock or ends at once; the deadline is met only by
	// a Bind stuck elsewhere.
	deadline := time.Now().Add(waitBound)
	stop := func() bool {
		select {
		case err := <-done:
			bound = true

			if err != nil {
				t.Errorf("Bind = %v", err)
			}

			return true
		default:
			return time.Now().After(deadline)
		}
	}

	if !waitingInLock("engine.(*Engine).limitAndPublish", stop) {
		if !bound {
			g.open()
			t.Fatal("the Bind neither waits on the raise lock nor ends")
		}

		t.Errorf("Bind ended, %v bound, while a raise held the journal's raise lock", bound)
	}

	g.open()
	s.e.WaitRaises(t)

	if !bound {
		if err := <-done; err != nil {
			t.Fatalf("Bind = %v", err)
		}
	}

	if got, want := limitOf(t, s, third.ID), s.j.Stats().VersionLimit.Counter; got < want {
		t.Errorf("the bound share holds %d, below the journal's limit %d", got, want)
	}
}

// A Bind waits in its own limit write; a raise woken meanwhile waits for
// the publish and gives the journal no limit above the new share's.
func TestRaiseWaitsForBindToPublish(t *testing.T) {
	s := newRaiser(t, time.Second, withSpare())
	third := s.spare
	g := newGate(t)
	s.lc.hold.Store(&heldRaise{
		g:     g,
		share: third.ID,
	})

	done := bindInBackground(t, s, third)
	// Registered after the Bind's wait, so it runs first and frees the Bind
	// should the test end early.
	t.Cleanup(g.open)
	waitForRaise(t, g)

	drawWhileHeld(t, s, 125, g)

	moved := func() bool {
		return len(marks(s)) > 1
	}

	// The raise woken by the draws reaches the lock at once; the deadline
	// is met only when no raise was woken at all.
	deadline := time.Now().Add(waitBound)
	stop := func() bool {
		return moved() || time.Now().After(deadline)
	}

	if !waitingInLock("engine.(*Engine).raiseOnce", stop) {
		g.open()

		if !moved() {
			t.Fatal("no raise reached the store")
		}

		t.Errorf("a raise gave the journal a limit while a Bind held the raise lock")
	}

	g.open()

	if err := <-done; err != nil {
		t.Fatalf("Bind = %v", err)
	}

	s.e.WaitRaises(t)

	if got, want := limitOf(t, s, third.ID), s.j.Stats().VersionLimit.Counter; got < want {
		t.Errorf("the bound share holds %d, below the journal's limit %d", got, want)
	}
}

// A share bound above the journal's limit moves the counter past it; the
// Bind's own raise counts from the counter, so the other shares write again
// after it.
func TestRaiseCountsFromTheCounter(t *testing.T) {
	s := newRaiser(t, time.Second, withSpare())
	l0 := marks(s)[0].Counter
	third := s.spare
	setLimit(t, s, third.ID, l0+3*testBlock)

	if err := s.e.Bind(t.Context(), third); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	m := marks(s)

	if len(m) != 2 {
		t.Fatalf("the journal was given %d limits, want the start's and the Bind's", len(m))
	}

	if next := s.j.Stats().NextVersion.Counter; m[1].Counter <= next {
		t.Errorf("the Bind's raise passed %d, not above the counter %d", m[1].Counter, next)
	}

	if _, err := s.e.Draw(t.Context(), s.one.ID, drawFile); err != nil {
		t.Errorf("another share's draw after the Bind's raise = %v", err)
	}
}

// A share created after Start, with a file committed several blocks above
// the limit Start passed, has its floor there; its Bind moves the counter
// past that limit and runs the raise itself, the new share among the shares
// raised, so its first write is served.
func TestLateShareBindRaisesItself(t *testing.T) {
	s := newRaiser(t, time.Second)
	ctx := t.Context()
	l0 := marks(s)[0].Counter
	late := enginetest.AddShare(t, s.fs.Store, "late", 1<<30)

	grant := entity.ShareGrant{
		Share:     late.ID,
		Principal: s.fs.Admin.Principal,
		Access:    entity.AccessAdmin,
	}

	if err := s.fs.Store.ControlPlane().SetGrant(ctx, grant); err != nil {
		t.Fatal(err)
	}

	root, _, err := s.fs.Store.Files().Root(ctx, s.fs.Admin, late.ID)
	if err != nil {
		t.Fatal(err)
	}

	_, file := s.fs.Create(t, s.fs.Admin, root, "f", kvstore.Regular(0o644))
	high := entity.VersionOf(l0 + 3*testBlock)
	pending := []kvstore.PendingExistence{
		{
			Share: late.ID,
			File:  file.ID,
			Ops: []kvstore.PendingOp{
				{
					Version: high,
					Kind:    kvstore.OpWrite,
					Extent:  entity.Extent{Length: 4096},
					Time:    s.clock.Now(),
				},
			},
		},
	}

	failed, err := s.fs.Store.Existence().Commit(ctx, pending)
	if err != nil || failed[file.ID] != nil {
		t.Fatalf("commit the late share's file at %d: %v, %v", high.Counter(), err, failed[file.ID])
	}

	if err := s.e.Bind(ctx, late); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	m := marks(s)

	if len(m) != 2 || m[1].Counter <= high.Counter() {
		t.Fatalf("limits %v, want a second one above the floor %d", m, high.Counter())
	}

	for _, id := range []entity.ShareID{s.one.ID, s.two.ID, late.ID} {
		if got := limitOf(t, s, id); got < m[1].Counter {
			t.Errorf("share %v holds %d below the raised limit %d", id, got, m[1].Counter)
		}
	}

	_, err = s.e.Write(
		ctx,
		late.ID,
		entity.FileID{0xda},
		0,
		[]byte("x"),
		engine.WriteOpts{
			Done: func() {
			},
		},
	)
	if err != nil {
		t.Errorf("the late share's first write = %v", err)
	}
}

// The raise falls due on the draw that crosses the trigger, with the clock
// standing still: the trigger is evaluated on every version, never on a
// timer.
func TestRaiseDueOnNextDraw(t *testing.T) {
	s := newRaiser(t, time.Second)
	l0 := marks(s)[0].Counter

	drawAtRate(t, s, 10, 50)
	drawN(t, s, 122)
	s.e.WaitRaises(t)

	if n := len(marks(s)); n != 1 {
		t.Fatalf("raised with %d versions left at a peak of 122 a second: %d limits", left(s), n)
	}

	drawN(t, s, 1)
	s.e.WaitRaises(t)

	if got, want := s.j.Stats().VersionLimit.Counter, l0+testBlock; got != want {
		t.Errorf(
			"the journal's limit is %d after the draw that crossed the trigger, want %d",
			got,
			want,
		)
	}
}

// The margin counts the existence age: at a steady 60 versions a second and
// an existence age of 10 s, the raise falls due below 2460 versions left,
// above the 2048 of a margin without it: on the 1637th version, not before,
// and only once.
func TestMarginCountsExistenceAge(t *testing.T) {
	s := newRaiser(t, 10*time.Second)

	drawAtRate(t, s, 60, 1636)
	s.e.WaitRaises(t)

	if n := len(marks(s)); n != 1 {
		t.Fatalf("raised with %d versions left: %d limits", left(s), n)
	}

	drawN(t, s, 1)
	s.e.WaitRaises(t)

	if n := len(marks(s)); n != 2 {
		t.Errorf("%d limits given on the version that left 2459 at 60 a second, want 2", n)
	}
}

// With a rate term of two blocks, each raise lifts the limit by one block
// from the limit last passed, never from the counter alone.
func TestRaiseFromLimitWhenRateTermAboveBlock(t *testing.T) {
	s := newRaiser(t, time.Second)
	l0 := marks(s)[0].Counter

	drawN(t, s, 125)
	s.e.WaitRaises(t)
	drawN(t, s, 124)
	s.e.WaitRaises(t)

	m := marks(s)

	if len(m) != 3 {
		t.Fatalf("the journal was given %d limits, want the start's and two raises", len(m))
	}

	if m[1].Counter != l0+testBlock || m[2].Counter != m[1].Counter+testBlock {
		t.Errorf(
			"raised to %d then %d from %d, want one block each",
			m[1].Counter,
			m[2].Counter,
			l0,
		)
	}
}

// A Bind whose Share leaves the counter within the trigger raises every
// served share's limit and its own before it gives the journal the limit
// and before it publishes.
func TestBindRaisesEveryShareWithinTrigger(t *testing.T) {
	s := newRaiser(t, time.Second, withSpare())
	l0 := marks(s)[0].Counter
	third := s.spare
	setLimit(t, s, third.ID, l0-100)
	g := newGate(t)
	s.j.limitGate.Store(g)

	done := bindInBackground(t, s, third)
	// Registered after the Bind's wait, so it runs first and frees the Bind
	// should the test end early.
	t.Cleanup(g.open)

	select {
	case err := <-done:
		t.Fatalf("Bind ended (%v) without giving the journal a raised limit", err)
	case <-g.entered:
	}

	if err := s.e.BindingOf(third.ID); !isDelay(err, vfserr.DelayFrozen) {
		t.Errorf("the share is published before the journal has its limit: %v", err)
	}

	mark := s.j.mark().Counter

	for _, id := range []entity.ShareID{s.one.ID, s.two.ID, third.ID} {
		if got := limitOf(t, s, id); got < mark {
			t.Errorf("share %v holds %d below the limit the journal is given, %d", id, got, mark)
		}
	}

	g.open()

	if err := <-done; err != nil {
		t.Fatalf("Bind = %v", err)
	}
}

// A removed binding in the table does not stop the raise of the others.
func TestRaiseSkipsRemovedBinding(t *testing.T) {
	removed := entity.NewShareID()
	s := newStarter(t)
	s.lc.removed = map[entity.ShareID]entity.TagRecord{
		removed: {
			Tag:   99,
			State: entity.TagOffloaded,
		},
	}
	s.e.SetBlockForTest(testBlock)
	s.e.TrackRaisesForTest()
	applyAge(t, s, time.Second)

	if err := s.start(t); err != nil {
		t.Fatal(err)
	}

	drawN(t, s, 125)
	s.e.WaitRaises(t)

	m := marks(s)

	if len(m) != 2 {
		t.Fatalf("the journal was given %d limits, want 2", len(m))
	}

	for _, id := range []entity.ShareID{s.one.ID, s.two.ID} {
		if got := limitOf(t, s, id); got < m[1].Counter {
			t.Errorf("share %v holds %d below the raised limit %d", id, got, m[1].Counter)
		}
	}
}

// A raise that fails is retried, waiting 10 ms, doubling to at most 1 s, and
// from 10 ms again once one commits; each failure is logged at Warn.
func TestRaiseRetriesWithBackoff(t *testing.T) {
	s := newRaiser(t, time.Second)
	fail := errors.New("store down")
	plan := make([]error, 0, 12)

	for range 9 {
		plan = append(plan, fail)
	}

	plan = append(plan, nil, fail, nil)
	s.lc.failRaises(plan...)
	base := len(s.lc.raiseCalls())

	calls := func() int {
		return len(s.lc.raiseCalls()) - base
	}

	// The raise fails and waits on the clock to retry; a draw must not wait
	// with it.
	g := newGate(t)
	drawWhileHeld(t, s, 125, g)

	waits := []time.Duration{10, 20, 40, 80, 160, 320, 640, 1000, 1000}

	for i, w := range waits {
		s.e.WaitRaises(t)

		if calls() != i+1 || s.clock.Pending() != 1 {
			t.Fatalf(
				"after attempt %d: %d attempts, %d waits, want %d and 1",
				i+1,
				calls(),
				s.clock.Pending(),
				i+1,
			)
		}

		s.clock.Advance((w - 1) * time.Millisecond)
		s.e.WaitRaises(t)

		if calls() != i+1 {
			t.Fatalf("attempt %d retried before its %d ms wait", i+1, w)
		}

		if i == len(waits)-1 {
			s.lc.hold.Store(&heldRaise{
				g: g,
			})
		}

		s.clock.Advance(time.Millisecond)
	}

	// Enough versions during the committing raise that the journal is still
	// within the trigger after it, so the raise runs once more.
	waitForRaise(t, g)
	drawWhileHeld(t, s, 250, g)
	g.open()
	s.e.WaitRaises(t)

	if calls() != 11 || s.clock.Pending() != 1 {
		t.Fatalf(
			"after the raise committed and ran again: %d attempts, %d waits, want 11 and 1",
			calls(),
			s.clock.Pending(),
		)
	}

	s.clock.Advance(9 * time.Millisecond)
	s.e.WaitRaises(t)

	if calls() != 11 {
		t.Fatal("the retry after a committed raise did not wait 10 ms")
	}

	s.clock.Advance(time.Millisecond)
	s.e.WaitRaises(t)

	if calls() != 12 {
		t.Errorf("%d attempts, want 12: the wait did not start again from 10 ms", calls())
	}

	if n := len(marks(s)); n != 3 {
		t.Errorf("the journal was given %d limits, want the start's and two raises", n)
	}

	if n := len(logsAt(s, slog.LevelWarn)); n != 10 {
		t.Errorf("%d failed raises logged at Warn, want 10", n)
	}
}

// A limit the journal refuses is not retried and is logged once at Error;
// the versions issued past the trigger after it wake no raise, and a write
// at the limit is answered retry-later, never as a full device.
func TestRefusedLimitIsNotRetried(t *testing.T) {
	s := newRaiser(t, time.Second)
	base := len(s.lc.raiseCalls())
	s.j.mu.Lock()
	s.j.fail = map[string]error{
		"limit": journal.ErrUnknownFormat,
	}
	s.j.mu.Unlock()

	drawN(t, s, 125)
	s.e.WaitRaises(t)

	if s.clock.Pending() != 0 {
		t.Errorf("a refused limit waits to be retried: %d waits", s.clock.Pending())
	}

	started := s.e.RaisesStarted()

	var err error

	for err == nil {
		_, err = s.e.Draw(t.Context(), s.one.ID, drawFile)
	}

	s.e.WaitRaises(t)

	if n := s.e.RaisesStarted() - started; n != 0 {
		t.Errorf("%d raises started for versions issued after the refusal, want none", n)
	}

	if n := len(s.lc.raiseCalls()) - base; n != 1 {
		t.Errorf("%d raises reached the store after the refusal and the draws after it, want 1", n)
	}

	if n := len(logsAt(s, slog.LevelError)); n != 1 {
		t.Errorf("%d records at Error for the refused limit, want 1", n)
	}

	var full *journal.NoSpaceError

	if !isDelay(err, vfserr.DelayCommit) || errors.As(err, &full) {
		t.Errorf("a draw at the limit = %v, want ErrDelay with DelayCommit", err)
	}
}

// A raise waiting to retry when a Bind's raise has the journal refuse a
// limit ends without another store call or log: the refusal persists.
func TestRefusalEndsARaiseWaitingToRetry(t *testing.T) {
	s := newRaiser(t, time.Second, withSpare())
	l0 := marks(s)[0].Counter
	s.lc.failRaises(errors.New("store down"))

	// The raise fails and waits on the clock to retry; a draw must not wait
	// with it.
	drawWhileHeld(t, s, 125, newGate(t))
	s.e.WaitRaises(t)

	if s.clock.Pending() != 1 {
		t.Fatalf("the failed raise is not waiting to retry: %d waits", s.clock.Pending())
	}

	s.j.mu.Lock()
	s.j.fail = map[string]error{
		"limit": journal.ErrUnknownFormat,
	}
	s.j.mu.Unlock()
	setLimit(t, s, s.spare.ID, l0+3*testBlock)

	if err := s.e.Bind(t.Context(), s.spare); err == nil {
		t.Fatal("Bind passed a limit the journal refuses")
	}

	base := len(s.lc.raiseCalls())
	s.clock.Advance(10 * time.Millisecond)
	s.e.WaitRaises(t)

	if n := len(s.lc.raiseCalls()) - base; n != 0 || s.clock.Pending() != 0 {
		t.Errorf("the raise ran on after the refusal: %d calls, %d waits", n, s.clock.Pending())
	}

	for _, r := range logsAt(s, slog.LevelError) {
		if strings.Contains(r.Message, "version limit raise refused") {
			t.Errorf("the waiting raise logged the refusal: %q", r.Message)
		}
	}
}

// A wake during a raise, after the raise read the counter, runs the raise
// once more when the versions drawn meanwhile leave the journal within the
// trigger after it.
func TestWakeDuringRaiseRunsOnceMore(t *testing.T) {
	s := newRaiser(t, time.Second)
	g := newGate(t)
	s.lc.hold.Store(&heldRaise{
		g: g,
	})

	drawWhileHeld(t, s, 125, g)
	waitForRaise(t, g)
	drawWhileHeld(t, s, 135, g)
	g.open()
	s.e.WaitRaises(t)

	m := marks(s)

	if len(m) != 3 {
		t.Fatalf("the journal was given %d limits, want the start's and two raises", len(m))
	}

	if m[2].Counter != m[1].Counter+testBlock {
		t.Errorf("the second raise passed %d, want %d", m[2].Counter, m[1].Counter+testBlock)
	}
}

// A wake during a raise runs no second raise when the first leaves the
// journal above the trigger.
func TestWakeDuringRaiseNoSecondWhenCovered(t *testing.T) {
	s := newRaiser(t, time.Second)
	g := newGate(t)
	s.lc.hold.Store(&heldRaise{
		g: g,
	})

	drawWhileHeld(t, s, 125, g)
	waitForRaise(t, g)
	drawWhileHeld(t, s, 10, g)
	g.open()
	s.e.WaitRaises(t)

	if n := len(marks(s)); n != 2 {
		t.Errorf("the journal was given %d limits, want the start's and one raise", n)
	}
}

// The margin is read from the settings at each check: an existence age
// applied while serving makes the raise due on the next draw.
func TestMarginReadOnEveryCheck(t *testing.T) {
	s := newRaiser(t, time.Second)

	drawAtRate(t, s, 60, 1896)
	s.e.WaitRaises(t)

	if n := len(marks(s)); n != 1 {
		t.Fatalf("raised with %d versions left at 60 a second and a 1 s existence age", left(s))
	}

	applyAge(t, s, 20*time.Second)
	drawN(t, s, 1)
	s.e.WaitRaises(t)

	if n := len(marks(s)); n != 2 {
		t.Errorf("the 20 s existence age applied did not make the raise due: %d limits", n)
	}
}

// With no settings applied the margin counts RFC 13's proposed existence age
// of 1 s: at 70 versions a second the raise falls due below 2240 left, on
// the 1857th version, and only once.
func TestMarginDefaultWithoutSettings(t *testing.T) {
	s := newRaiser(t, 0)

	drawAtRate(t, s, 70, 1856)
	s.e.WaitRaises(t)

	if n := len(marks(s)); n != 1 {
		t.Fatalf("raised with %d versions left: %d limits", left(s), n)
	}

	drawN(t, s, 1)
	s.e.WaitRaises(t)

	if n := len(marks(s)); n != 2 {
		t.Errorf("%d limits given on the version that left 2239 at 70 a second, want 2", n)
	}
}

// The request that issues a version past the trigger returns without
// waiting for the raise: the raise never runs on its stack, and a request
// made while a raise waits on the store does not wait.
func TestRequestNeverWaitsForRaise(t *testing.T) {
	s := newRaiser(t, time.Second)
	g := newGate(t)
	s.lc.hold.Store(&heldRaise{
		g: g,
	})
	drew := make(chan struct{})

	go drawPastTrigger(t, s, drew)

	waitForRaise(t, g)

	calls := s.lc.raiseCalls()

	if strings.Contains(calls[len(calls)-1].stack, "drawPastTrigger") {
		t.Errorf("the raise runs on the request's stack")
	}

	done := make(chan struct{})

	go requestDuringRaise(t, s, done)

	waits := requestWaits(done)

	if waits {
		t.Errorf("a request waits while a raise waits on the store")
	}

	g.open()
	<-drew

	// A request that waited may never return; the test does not wait for
	// it then, and the profiler reports the goroutine left behind.
	if !waits {
		<-done
	}

	s.e.WaitRaises(t)
}

// drawPastTrigger draws versions until the raise is due.
func drawPastTrigger(t *testing.T, s *starter, done chan<- struct{}) {
	defer close(done)

	for range 125 {
		if _, err := s.e.Draw(context.Background(), s.one.ID, drawFile); err != nil {
			t.Errorf("draw: %v", err)

			return
		}
	}
}

// requestDuringRaise draws a version and writes a byte past the trigger.
func requestDuringRaise(t *testing.T, s *starter, done chan<- struct{}) {
	defer close(done)

	if _, err := s.e.Draw(context.Background(), s.one.ID, drawFile); err != nil {
		t.Errorf("draw: %v", err)
	}

	_, err := s.e.Write(
		context.Background(),
		s.one.ID,
		entity.FileID{0xd8},
		0,
		[]byte("x"),
		engine.WriteOpts{
			Done: func() {
			},
		},
	)
	if err != nil {
		t.Errorf("write: %v", err)
	}
}

// requestWaits reports whether the requestDuringRaise goroutine is seen in
// a wait state before done closes.
func requestWaits(done <-chan struct{}) bool {
	buf := make([]byte, 1<<20)

	for {
		select {
		case <-done:
			return false
		default:
		}

		n := runtime.Stack(buf, true)

		for g := range strings.SplitSeq(string(buf[:n]), "\n\n") {
			if strings.Contains(g, "engine_test.requestDuringRaise") && waiting(g) {
				return true
			}
		}

		runtime.Gosched()
	}
}

// Start writes the limit a block above the next version with the block the
// engine has, the one the raise uses too.
func TestStartUsesBlockField(t *testing.T) {
	s := newStarter(t)
	s.e.SetBlockForTest(64)

	if err := s.start(t); err != nil {
		t.Fatal(err)
	}

	if got, want := limitOf(t, s, s.one.ID), s.j.Stats().NextVersion.Counter+64; got != want {
		t.Errorf("start wrote %d, want %d", got, want)
	}
}

// The journals handed to the engine by hand carry the limit the journal
// holds, so a write far below it wakes no raise.
func TestSetJournalsForTestSeedsTheLimit(t *testing.T) {
	u := newUnitWith(t, unitConfig{
		unbound: true,
	})
	u.e.TrackRaisesForTest()

	if err := u.e.Bind(t.Context(), u.share); err != nil {
		t.Fatal(err)
	}

	_, err := u.e.Write(
		t.Context(),
		u.share.ID,
		entity.FileID{0xd9},
		0,
		[]byte("x"),
		engine.WriteOpts{
			Done: func() {
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	u.e.WaitRaises(t)

	if got := u.j.Stats().VersionLimit.Counter; got != unitLimit {
		t.Errorf("the fake's limit is %d after one write, want %d", got, uint64(unitLimit))
	}
}

// With three times K shares served, one raise makes one store call that
// names every one, a share being removed included, and the others get the
// limit.
func TestOneRaiseCarriesEveryShare(t *testing.T) {
	s := newRaiser(t, time.Second, withShares(19), withSmallK())
	all := append([]engine.Share{s.one, s.two}, s.more...)
	removing := s.more[0].ID

	err := s.fs.Store.ControlPlane().DeleteShare(t.Context(), removing, entity.SnapshotsRefuse)
	if err != nil {
		t.Fatal(err)
	}

	base := len(s.lc.raiseCalls())

	drawN(t, s, 125)
	s.e.WaitRaises(t)

	calls := s.lc.raiseCalls()[base:]

	if len(calls) != 1 {
		t.Fatalf("one raise made %d store calls, want 1", len(calls))
	}

	for _, sh := range all {
		if !slices.Contains(calls[0].shares, sh.ID) {
			t.Errorf("the raise's store call does not name share %v", sh.ID)
		}
	}

	mark := marks(s)[len(marks(s))-1].Counter

	for _, sh := range all {
		if sh.ID == removing {
			continue
		}

		if got := limitOf(t, s, sh.ID); got < mark {
			t.Errorf("share %v holds %d below the raised limit %d", sh.ID, got, mark)
		}
	}
}

// smallKV is an in-memory kv.KV whose entry limit makes the store's key
// budget K seven keys a batch.
func smallKV(tb testing.TB) kv.KV {
	return limitedKV{
		KV: memKV(tb),
	}
}

// limitedKV is a kv.KV reporting a transaction entry limit of 12, which the
// store's key budget turns into K = 7.
type limitedKV struct {
	kv.KV
}

func (l limitedKV) Limits() kv.TxnLimits {
	lim := l.KV.Limits()
	lim.TxnEntries = 12

	return lim
}
