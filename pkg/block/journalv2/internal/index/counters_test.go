package index

import (
	"errors"
	"math"
	"math/big"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

func ver(counter uint64) core.Version { return core.Version{Counter: counter} }

// limited returns counters whose version limit is limit.
func limited(limit uint64) *Counters {
	c := &Counters{}
	c.SetVersionLimit(ver(limit))
	return c
}

// mustDraw draws a version and fails the test on a refusal.
func mustDraw(t *testing.T, c *Counters) core.Version {
	t.Helper()
	got, err := c.Draw()
	if err != nil {
		t.Fatalf("Draw: %v", err)
	}
	return got
}

// assertVersionRefusal fails unless err is the version limit's refusal.
func assertVersionRefusal(t *testing.T, err error) {
	t.Helper()
	var ns *core.NoSpaceError
	if !errors.As(err, &ns) || !errors.Is(err, core.ErrNoSpace) || ns.Limit != core.LimitVersion || ns.Missing != 1 || ns.Unit != core.UnitVersions {
		t.Fatalf("want a NoSpaceError naming the version limit, got %v", err)
	}
}

// TestNumbersCompare checks precedence: the greater (version, seq) pair wins,
// version first, a version compared as one unsigned 128-bit number with the
// epoch half on top. It catches seq compared before version, and the counter
// half compared before the epoch half.
func TestNumbersCompare(t *testing.T) {
	for _, c := range []struct {
		name string
		a, b Numbers
		want int
	}{
		{"higher version beats higher seq", Numbers{ver(2), 1}, Numbers{ver(1), 9}, 1},
		{"equal versions: higher seq wins", Numbers{ver(5), 3}, Numbers{ver(5), 4}, -1},
		{"epoch outranks every counter", Numbers{core.Version{Epoch: 1}, 0}, Numbers{ver(math.MaxUint64), math.MaxUint64}, 1},
		{"equal", Numbers{core.Version{Epoch: 2, Counter: 7}, 3}, Numbers{core.Version{Epoch: 2, Counter: 7}, 3}, 0},
	} {
		if got := c.a.Compare(c.b); got != c.want {
			t.Errorf("%s: %v.Compare(%v) = %d, want %d", c.name, c.a, c.b, got, c.want)
		}
		if got := c.b.Compare(c.a); got != -c.want {
			t.Errorf("%s: reversed: got %d, want %d", c.name, got, -c.want)
		}
	}

	// Against an oracle that reads the pair as one 192-bit number, epoch,
	// then counter, then seq. Values are drawn from a small set so that equal
	// halves, which decide the tie-breaks, come up often.
	edge := []uint64{0, 1, 2, math.MaxUint64 - 1, math.MaxUint64}
	pick := func(r *rand.Rand) uint64 { return edge[r.IntN(len(edge))] }
	oracle := func(n Numbers) *big.Int {
		x := new(big.Int).SetUint64(n.Version.Epoch)
		x.Lsh(x, 64).Or(x, new(big.Int).SetUint64(n.Version.Counter))
		x.Lsh(x, 64).Or(x, new(big.Int).SetUint64(uint64(n.Seq)))
		return x
	}
	r := rand.New(rand.NewPCG(1, 2))
	for range 20000 {
		a := Numbers{core.Version{Epoch: pick(r), Counter: pick(r)}, core.Seq(pick(r))}
		b := Numbers{core.Version{Epoch: pick(r), Counter: pick(r)}, core.Seq(pick(r))}
		if got, want := a.Compare(b), oracle(a).Cmp(oracle(b)); got != want {
			t.Fatalf("%v.Compare(%v) = %d, oracle %d", a, b, got, want)
		}
	}
}

// TestNothingIssuedBeforeLimit checks that a journal with no version limit
// issues no version. It catches an unset limit read as no limit at all.
func TestNothingIssuedBeforeLimit(t *testing.T) {
	var c Counters
	_, err := c.Draw()
	assertVersionRefusal(t, err)
	if got := c.NextVersion(); got != ver(1) {
		t.Errorf("NextVersion = %v, want %v", got, ver(1))
	}
	if got := c.VersionsLeft(); got != 0 {
		t.Errorf("VersionsLeft = %d, want 0", got)
	}
}

// TestDrawJustBelowAndAtLimit checks that versions are issued strictly below
// the limit, that the refusal names it, and that a refused draw consumes no
// version. It catches an inclusive limit and a refusal that still advances the
// counter.
func TestDrawJustBelowAndAtLimit(t *testing.T) {
	c := limited(4)
	for want := uint64(1); want <= 3; want++ {
		if left := c.VersionsLeft(); left != 4-want {
			t.Fatalf("before draw %d: VersionsLeft = %d, want %d", want, left, 4-want)
		}
		if got := mustDraw(t, c); got != ver(want) {
			t.Fatalf("Draw = %v, want %v", got, ver(want))
		}
	}
	if left := c.VersionsLeft(); left != 0 {
		t.Fatalf("at the limit: VersionsLeft = %d, want 0", left)
	}
	_, err := c.Draw()
	assertVersionRefusal(t, err)
	if got := c.VersionLimit(); got != ver(4) {
		t.Errorf("VersionLimit = %v, want %v", got, ver(4))
	}

	c.SetVersionLimit(ver(5))
	if got := mustDraw(t, c); got != ver(4) {
		t.Fatalf("after raising the limit: Draw = %v, want %v", got, ver(4))
	}
}

// TestLoweredLimitRefuses checks that the limit is set, not raised: the
// smallest of the shares' limits can fall when a share attaches. It catches
// the limit kept as a maximum, and VersionsLeft underflowing once the counter
// stands above the limit.
func TestLoweredLimitRefuses(t *testing.T) {
	c := limited(100)
	for range 10 {
		mustDraw(t, c)
	}
	c.SetVersionLimit(ver(5))
	_, err := c.Draw()
	assertVersionRefusal(t, err)
	if left := c.VersionsLeft(); left != 0 {
		t.Errorf("VersionsLeft = %d, want 0", left)
	}
}

// TestShareFloorRaisesCounter is the attached-share check at the index: a
// share whose files hold versions far above the counter raises it before any
// write through the share draws a version. It catches a floor assigned rather
// than raised to, which a later, lower floor would pull back down.
func TestShareFloorRaisesCounter(t *testing.T) {
	c := limited(math.MaxUint64)
	if err := c.RaiseFloor(ver(1000)); err != nil {
		t.Fatal(err)
	}
	if got := mustDraw(t, c); got != ver(1001) {
		t.Fatalf("Draw = %v, want %v", got, ver(1001))
	}

	const shareFloor = 900_000_000
	if err := c.RaiseFloor(ver(shareFloor)); err != nil {
		t.Fatal(err)
	}
	if got := mustDraw(t, c); got.Compare(ver(shareFloor)) <= 0 {
		t.Fatalf("Draw = %v, want above the share's floor %d", got, shareFloor)
	}

	if err := c.RaiseFloor(ver(2000)); err != nil {
		t.Fatal(err)
	}
	if got := mustDraw(t, c); got.Compare(ver(shareFloor)) <= 0 {
		t.Fatalf("a lower floor pulled the counter back: Draw = %v", got)
	}
}

// TestNonZeroEpochRefused checks that a floor or a record with a non-zero
// epoch is refused as an unknown format and changes nothing, and that a limit
// with one stands above every version this format issues. It catches the epoch
// ignored, and a record's sequence number taken before its version is refused.
func TestNonZeroEpochRefused(t *testing.T) {
	c := limited(100)
	mustDraw(t, c)
	if _, err := c.NextSeq(); err != nil {
		t.Fatal(err)
	}
	wantVersion, wantLeft := c.NextVersion(), c.VersionsLeft()

	if err := c.RaiseFloor(core.Version{Epoch: 1, Counter: 50}); !errors.Is(err, core.ErrUnknownFormat) {
		t.Errorf("RaiseFloor with epoch 1: got %v, want ErrUnknownFormat", err)
	}
	if err := c.Observe(Numbers{core.Version{Epoch: 1, Counter: 50}, 50}); !errors.Is(err, core.ErrUnknownFormat) {
		t.Errorf("Observe with epoch 1: got %v, want ErrUnknownFormat", err)
	}
	if got := c.NextVersion(); got != wantVersion {
		t.Errorf("NextVersion moved to %v, want %v", got, wantVersion)
	}
	if got := c.VersionsLeft(); got != wantLeft {
		t.Errorf("VersionsLeft moved to %d, want %d", got, wantLeft)
	}
	if got, err := c.NextSeq(); err != nil || got != 2 {
		t.Errorf("NextSeq = %d, %v; want 2: the refused record's seq was taken", got, err)
	}

	c.SetVersionLimit(core.Version{Epoch: 1})
	if got := mustDraw(t, c); got != ver(2) {
		t.Errorf("under an epoch-1 limit: Draw = %v, want %v", got, ver(2))
	}
	if got := c.VersionsLeft(); got != math.MaxUint64-3 {
		t.Errorf("under an epoch-1 limit: VersionsLeft = %d, want %d", got, uint64(math.MaxUint64-3))
	}
}

// TestNoVersionRepeats is the version-repeat check at the index. A version
// handed out before its record synced is lost from disk by a crash; a counter
// restarted from the disk maximum alone issues it again, and one restarted
// above the durable limit, which the caller passes as part of the floor, does
// not. It catches recovery that ignores the floor.
func TestNoVersionRepeats(t *testing.T) {
	const limit = 100
	before := limited(limit)
	for range 5 { // synced: versions 1 to 5 are on disk
		mustDraw(t, before)
	}
	lost := mustDraw(t, before) // returned to the client, then lost by a crash

	diskMax := Numbers{ver(5), 5}

	// The control: a counter that restarts from what is on disk.
	naive := limited(2 * limit)
	if err := naive.Observe(diskMax); err != nil {
		t.Fatal(err)
	}
	if got := mustDraw(t, naive); got != lost {
		t.Fatalf("control: a counter restarted from the disk maximum drew %v, expected it to reissue %v", got, lost)
	}

	after := &Counters{}
	if err := after.Observe(diskMax); err != nil {
		t.Fatal(err)
	}
	if err := after.RaiseFloor(ver(limit)); err != nil {
		t.Fatal(err)
	}
	// Reopened, recovered, and still no limit set: nothing is issued yet.
	_, err := after.Draw()
	assertVersionRefusal(t, err)
	after.SetVersionLimit(ver(limit + 3))
	for range 2 {
		if got := mustDraw(t, after); got.Compare(lost) <= 0 {
			t.Fatalf("Draw after recovery = %v, not above the lost %v", got, lost)
		}
	}
	_, err = after.Draw()
	assertVersionRefusal(t, err)
	after.SetVersionLimit(ver(2 * limit))
	if got := mustDraw(t, after); got != ver(limit+3) {
		t.Fatalf("after raising the limit: Draw = %v, want %v", got, ver(limit+3))
	}
}

// TestObserveAnyOrder checks that records seen in any order leave both
// counters above the greatest of each, which is what lets recovery read
// segments in any order. It catches Observe assigning rather than raising.
func TestObserveAnyOrder(t *testing.T) {
	c := limited(math.MaxUint64)
	for _, n := range []Numbers{{ver(40), 7}, {ver(90), 3}, {ver(10), 80}, {ver(20), 20}} {
		if err := c.Observe(n); err != nil {
			t.Fatal(err)
		}
	}
	if got := mustDraw(t, c); got != ver(91) {
		t.Errorf("Draw = %v, want %v", got, ver(91))
	}
	if got, err := c.NextSeq(); err != nil || got != 81 {
		t.Errorf("NextSeq = %d, %v; want 81", got, err)
	}
}

// TestDrawConsumesNoSeq checks that a draw issues a version and nothing else,
// and that the two counters are separate. It catches Draw taking a sequence
// number, or both kinds drawn from one counter.
func TestDrawConsumesNoSeq(t *testing.T) {
	c := limited(100)
	for range 5 {
		mustDraw(t, c)
	}
	if got, err := c.NextSeq(); err != nil || got != 1 {
		t.Errorf("NextSeq after five draws = %d, %v; want 1", got, err)
	}
	if got := mustDraw(t, c); got != ver(6) {
		t.Errorf("Draw after a NextSeq = %v, want %v", got, ver(6))
	}
}

// TestConcurrentDraw checks that concurrent draws never issue a number twice
// or skip one, issue in order within each caller, and never pass the limit.
// It catches a counter that is not atomic, and a limit check made apart from
// the increment.
func TestConcurrentDraw(t *testing.T) {
	const goroutines, each = 8, 2000

	run := func(draw func() (uint64, error)) (issued [][]uint64, refused int64) {
		issued = make([][]uint64, goroutines)
		var refusals atomic.Int64
		var wg sync.WaitGroup
		for g := range goroutines {
			wg.Go(func() {
				for range each {
					n, err := draw()
					if err != nil {
						refusals.Add(1)
						continue
					}
					issued[g] = append(issued[g], n)
				}
			})
		}
		wg.Wait()
		return issued, refusals.Load()
	}
	check := func(name string, issued [][]uint64, want uint64) {
		t.Helper()
		seen := make(map[uint64]bool)
		for g, ns := range issued {
			for i, n := range ns {
				if i > 0 && n <= ns[i-1] {
					t.Fatalf("%s: goroutine %d issued %d after %d", name, g, n, ns[i-1])
				}
				if seen[n] {
					t.Fatalf("%s: %d issued twice", name, n)
				}
				seen[n] = true
			}
		}
		for n := uint64(1); n <= want; n++ {
			if !seen[n] {
				t.Fatalf("%s: %d never issued", name, n)
			}
		}
		if uint64(len(seen)) != want {
			t.Fatalf("%s: issued %d numbers, want %d", name, len(seen), want)
		}
	}

	c := limited(math.MaxUint64)
	issued, _ := run(func() (uint64, error) { got, err := c.Draw(); return got.Counter, err })
	check("Draw", issued, goroutines*each)
	issued, _ = run(func() (uint64, error) { got, err := c.NextSeq(); return uint64(got), err })
	check("NextSeq", issued, goroutines*each)

	const limit = goroutines * each / 2
	c = limited(limit)
	issued, refused := run(func() (uint64, error) { got, err := c.Draw(); return got.Counter, err })
	check("Draw under a limit", issued, limit-1)
	if want := int64(goroutines*each - (limit - 1)); refused != want {
		t.Fatalf("Draw under a limit: %d refused, want %d", refused, want)
	}
}

// TestRaiseFloorConcurrentWithDraw checks that a raise racing draws never
// lowers the counter or is lost: floors are raised just above the counter while
// other goroutines draw past them, no version is issued twice, and a draw made
// after RaiseFloor returns is above its floor. It catches a raise that checks
// the counter, then stores over whatever draws issued in between.
func TestRaiseFloorConcurrentWithDraw(t *testing.T) {
	const goroutines, each = 8, 5000
	c := limited(math.MaxUint64)
	issued := make([][]uint64, goroutines+1)
	var stop atomic.Bool
	var drawers, raiser sync.WaitGroup
	for g := range goroutines {
		drawers.Go(func() {
			for range each {
				got, err := c.Draw()
				if err != nil {
					t.Error(err)
					return
				}
				issued[g] = append(issued[g], got.Counter)
			}
		})
	}
	raiser.Go(func() {
		for !stop.Load() {
			floor := c.NextVersion().Counter + 2
			if err := c.RaiseFloor(ver(floor)); err != nil {
				t.Error(err)
				return
			}
			got, err := c.Draw()
			if err != nil {
				t.Error(err)
				return
			}
			if got.Counter <= floor {
				t.Errorf("Draw after RaiseFloor(%d) returned %v", floor, got)
				return
			}
			issued[goroutines] = append(issued[goroutines], got.Counter)
		}
	})
	drawers.Wait()
	stop.Store(true)
	raiser.Wait()

	seen := make(map[uint64]bool)
	for _, ns := range issued {
		for _, n := range ns {
			if seen[n] {
				t.Fatalf("%d issued twice", n)
			}
			seen[n] = true
		}
	}
}

// TestConcurrentObserve checks that records observed concurrently, as a
// rebuild fed from several segments at once observes them, only ever raise
// the counters: a watcher sees NextVersion never fall while the observers
// race, and both counters end above the greatest number seen. It catches a
// raise that checks the counter, then stores over a greater value another
// observer raised it to in between.
func TestConcurrentObserve(t *testing.T) {
	const goroutines, each = 8, 20000
	c := limited(math.MaxUint64)
	var stop atomic.Bool
	var observers, watcher sync.WaitGroup
	watcher.Go(func() {
		last := c.NextVersion()
		for !stop.Load() {
			next := c.NextVersion()
			if next.Compare(last) < 0 {
				t.Errorf("NextVersion fell from %v to %v", last, next)
				return
			}
			last = next
		}
	})
	for g := range goroutines {
		observers.Go(func() {
			// Interleaved ascending numbers keep the observers racing on
			// neighbouring values for the whole run.
			for i := range each {
				n := uint64(i*goroutines + g + 1)
				if err := c.Observe(Numbers{ver(n), core.Seq(n)}); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	observers.Wait()
	stop.Store(true)
	watcher.Wait()

	if got := c.NextVersion(); got != ver(goroutines*each+1) {
		t.Errorf("NextVersion = %v, want %v", got, ver(goroutines*each+1))
	}
	if got, err := c.NextSeq(); err != nil || got != goroutines*each+1 {
		t.Errorf("NextSeq = %d, %v; want %d", got, err, goroutines*each+1)
	}
}

// TestExhaustionNeverWraps checks the top of both counters' ranges. A floor
// or a record carrying a number above every one the counters issue is refused
// when it arrives and changes nothing. The counters issue up to that top and
// then refuse rather than wrap to zero, which would outrank nothing and be
// issued again, and a journal recovered from the last numbers it issued
// reopens and refuses as before. It catches an overflowing increment, a
// counter let in where it could issue nothing, and a journal that refuses to
// reopen on its own records.
func TestExhaustionNeverWraps(t *testing.T) {
	c := &Counters{}
	c.SetVersionLimit(core.Version{Epoch: 1})
	for _, err := range []error{
		c.RaiseFloor(ver(math.MaxUint64)),
		c.Observe(Numbers{ver(math.MaxUint64), 1}),
		c.Observe(Numbers{ver(1), math.MaxUint64}),
	} {
		if !errors.Is(err, core.ErrUnknownFormat) {
			t.Errorf("a number above every one issued: got %v, want ErrUnknownFormat", err)
		}
	}
	if got := c.NextVersion(); got != ver(1) {
		t.Fatalf("a refused number moved NextVersion to %v", got)
	}
	if got, err := c.NextSeq(); err != nil || got != 1 {
		t.Fatalf("a refused number moved NextSeq: got %d, %v; want 1", got, err)
	}

	if err := c.RaiseFloor(ver(math.MaxUint64 - 2)); err != nil {
		t.Fatal(err)
	}
	if err := c.Observe(Numbers{ver(1), math.MaxUint64 - 2}); err != nil {
		t.Fatal(err)
	}
	lastVersion := mustDraw(t, c)
	if lastVersion != ver(math.MaxUint64-1) {
		t.Fatalf("Draw = %v, want %v", lastVersion, ver(math.MaxUint64-1))
	}
	_, err := c.Draw()
	assertVersionRefusal(t, err)
	if got := c.VersionsLeft(); got != 0 {
		t.Errorf("VersionsLeft = %d, want 0", got)
	}
	if got := c.NextVersion(); got != ver(math.MaxUint64) {
		t.Errorf("NextVersion = %v, want %v", got, ver(math.MaxUint64))
	}
	lastSeq, err := c.NextSeq()
	if err != nil || lastSeq != math.MaxUint64-1 {
		t.Fatalf("NextSeq = %d, %v; want %d", lastSeq, err, uint64(math.MaxUint64-1))
	}
	if got, err := c.NextSeq(); !errors.Is(err, ErrSeqExhausted) {
		t.Fatalf("NextSeq at the top of the counter: got %d, %v; want ErrSeqExhausted", got, err)
	}

	// Reopened on the last numbers it issued, with its last version as the
	// floor: the journal opens, and refuses as it did before.
	reopened := &Counters{}
	reopened.SetVersionLimit(core.Version{Epoch: 1})
	if err := reopened.Observe(Numbers{lastVersion, lastSeq}); err != nil {
		t.Fatalf("Observe of the last numbers issued: %v", err)
	}
	if err := reopened.RaiseFloor(lastVersion); err != nil {
		t.Fatalf("RaiseFloor to the last version issued: %v", err)
	}
	_, err = reopened.Draw()
	assertVersionRefusal(t, err)
	if _, err := reopened.NextSeq(); !errors.Is(err, ErrSeqExhausted) {
		t.Fatalf("reopened NextSeq: got %v, want ErrSeqExhausted", err)
	}
}
