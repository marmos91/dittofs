package sim

import (
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"
)

var (
	eio    = FaultAction{Kind: Fail, Err: syscall.EIO}
	enospc = FaultAction{Kind: Fail, Err: syscall.ENOSPC}
)

func newSchedule(t *testing.T, seed uint64, rules ...Rule) *Schedule {
	t.Helper()

	s, err := NewSchedule(seed, nil, rules...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// calls returns the actions s gives n calls of op on key.
func calls(s *Schedule, op, key string, n int) []FaultAction {
	got := make([]FaultAction, n)
	for i := range got {
		got[i] = s.At(op, []byte(key))
	}
	return got
}

// fires returns the 1-based numbers of the calls in got that did not pass.
func fires(got []FaultAction) []int {
	var n []int
	for i, a := range got {
		if a.Kind != Pass {
			n = append(n, i+1)
		}
	}
	return n
}

// mixed drives s with a fixed interleaving of ops and keys.
func mixed(s *Schedule) []FaultAction {
	ops := []string{"read", "write", "sync"}
	keys := []string{"seg-1", "seg-2", "format", "seg-3", "catalog"}

	got := make([]FaultAction, 1000)
	for i := range got {
		got[i] = s.At(ops[i%len(ops)], []byte(keys[i%len(keys)]))
	}
	return got
}

var mixedRules = []Rule{
	{Op: "read", Key: "seg-*", Prob: 0.3, Action: eio},
	{Op: "write", Key: "*", Prob: 0.2, Action: FaultAction{Kind: Tear, N: 3}},
	{Op: "*", Key: "*", Prob: 0.1, Action: FaultAction{Kind: Drop}},
}

func TestSameSeedAndRulesGiveTheSameActions(t *testing.T) {
	want := mixed(newSchedule(t, 7, mixedRules...))
	got := mixed(newSchedule(t, 7, mixedRules...))

	if !slices.Equal(got, want) {
		t.Fatal("two schedules with the same seed and rules gave different actions")
	}
}

func TestDifferentSeedGivesADifferentSequence(t *testing.T) {
	a := mixed(newSchedule(t, 7, mixedRules...))
	b := mixed(newSchedule(t, 8, mixedRules...))

	if slices.Equal(a, b) {
		t.Fatal("seeds 7 and 8 gave the same 1000 actions")
	}
}

func TestTransientFailureFailsTheFirstCallsThenPasses(t *testing.T) {
	s := newSchedule(t, 1, Rule{Op: "read", Key: "seg-7", Count: 3, Action: eio})

	if got := fires(calls(s, "read", "seg-7", 10)); !slices.Equal(got, []int{1, 2, 3}) {
		t.Fatalf("Count 3 failed calls %v, want [1 2 3]", got)
	}
}

func TestPermanentFailureFailsEveryCall(t *testing.T) {
	s := newSchedule(t, 1, Rule{Op: "read", Key: "seg-7", Action: eio})

	for i, a := range calls(s, "read", "seg-7", 100) {
		if a != eio {
			t.Fatalf("call %d: %v, want every call to fail with EIO", i+1, a)
		}
	}
}

func TestNthCallCountsOnlyItsOpAndKey(t *testing.T) {
	s := newSchedule(t, 1, Rule{Op: "sync", Key: "seg-7", From: 3, Count: 1, Action: eio})

	var hit []int
	for i := 1; i <= 5; i++ {
		// Calls on another key and another op on the same key, which the
		// rule's count must not see.
		if a := s.At("sync", []byte("seg-8")); a.Kind != Pass {
			t.Fatalf("a sync of seg-8 got %v", a)
		}
		if a := s.At("write", []byte("seg-7")); a.Kind != Pass {
			t.Fatalf("a write of seg-7 got %v", a)
		}
		if s.At("sync", []byte("seg-7")) == eio {
			hit = append(hit, i)
		}
	}

	if !slices.Equal(hit, []int{3}) {
		t.Fatalf("the rule for the 3rd sync of seg-7 fired on syncs %v, want [3]", hit)
	}
}

func TestFirstFiringRuleWinsAndEveryMatchingRuleCounts(t *testing.T) {
	s := newSchedule(t, 1,
		Rule{Op: "read", Key: "*", Count: 2, Action: eio},
		Rule{Op: "read", Key: "*", From: 2, Count: 2, Action: enospc},
	)

	// Call 2: both fire, the first wins. Call 3: the second fires, having
	// counted calls 1 and 2 although the first rule took them.
	want := []FaultAction{eio, eio, enospc, {}}
	if got := calls(s, "read", "seg-1", 4); !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestARulesDrawsDoNotDependOnOtherRules(t *testing.T) {
	p := Rule{Op: "write", Key: "*", Prob: 0.5, Action: eio}
	other := Rule{Op: "read", Key: "*", Prob: 0.5, Action: FaultAction{Kind: Drop}}

	alone := fires(calls(newSchedule(t, 3, p), "write", "seg-1", 200))
	behind := fires(calls(newSchedule(t, 3, other, p), "write", "seg-1", 200))

	if !slices.Equal(alone, behind) {
		t.Fatal("inserting a rule for another op ahead of a Prob rule changed the calls it fires on")
	}
}

func TestRulesSelectingTheSameCallsDrawIndependently(t *testing.T) {
	// The same calls, the same odds, different errors: the second must fire
	// on some call the first leaves alone.
	s := newSchedule(t, 3,
		Rule{Op: "read", Key: "*", Prob: 0.5, Action: eio},
		Rule{Op: "read", Key: "*", Prob: 0.5, Action: enospc},
	)

	if !slices.Contains(calls(s, "read", "seg-1", 200), enospc) {
		t.Fatal("a rule behind one with the same selection never fired: the two share a stream")
	}
}

func TestKeyPatterns(t *testing.T) {
	s := newSchedule(t, 1,
		Rule{Op: "read", Key: "seg-*", Action: eio},
		Rule{Op: "read", Key: "format", Action: enospc},
	)

	cases := map[string]FaultAction{
		"seg-7":      eio,
		"seg-":       eio,
		"format":     enospc,
		"format.tmp": {},
		"catalog":    {},
	}
	for key, want := range cases {
		if got := s.At("read", []byte(key)); got != want {
			t.Errorf("read of %q: %v, want %v", key, got, want)
		}
	}

}

func TestStarMatchesEveryKey(t *testing.T) {
	s := newSchedule(t, 1, Rule{Op: "read", Key: "*", Action: eio})

	for _, key := range []string{
		"seg-1",
		"",          // the key of list and sync_dir
		"shard/1",   // a '/' path.Match's star stops at
		"a\x2fb",    // a binary key holding the byte '/'
		"\xff/\xfe", // not valid UTF-8
	} {
		if got := s.At("read", []byte(key)); got != eio {
			t.Errorf("read of %q under Key \"*\": %v, want %v", key, got, eio)
		}
	}
}

func TestAStarCrossesSlashesAndBacktracks(t *testing.T) {
	s := newSchedule(t, 1,
		Rule{Op: "read", Key: "shard/*", Action: eio},
		Rule{Op: "read", Key: "*.seg", Action: enospc},
	)

	cases := map[string]FaultAction{
		"shard/1":   eio,
		"shard/a/b": eio,
		"shard/":    eio,
		"shard":     {},
		"shards/1":  {},
		"a.b.seg":   enospc, // the star must give back the first '.'
		"a.seg.tmp": {},
	}
	for key, want := range cases {
		if got := s.At("read", []byte(key)); got != want {
			t.Errorf("read of %q: %v, want %v", key, got, want)
		}
	}
}

func TestMoreThanOneStar(t *testing.T) {
	s := newSchedule(t, 1, Rule{Op: "read", Key: "a*b*c", Action: eio})

	cases := map[string]FaultAction{
		"abc":    eio, // each star takes nothing
		"aXbYc":  eio,
		"aXbYcZ": {}, // a byte after the last literal
		"acb":    {}, // the literals out of order
	}
	for key, want := range cases {
		if got := s.At("read", []byte(key)); got != want {
			t.Errorf("read of %q under Key \"a*b*c\": %v, want %v", key, got, want)
		}
	}
}

func TestProbability(t *testing.T) {
	half := len(fires(calls(newSchedule(t, 5, Rule{Op: "read", Key: "*", Prob: 0.5, Action: eio}), "read", "k", 1000)))
	if half < 400 || half > 600 {
		t.Fatalf("Prob 0.5 fired on %d of 1000 calls", half)
	}

	all := len(fires(calls(newSchedule(t, 5, Rule{Op: "read", Key: "*", Prob: 1, Action: eio}), "read", "k", 1000)))
	if all != 1000 {
		t.Fatalf("Prob 1 fired on %d of 1000 calls", all)
	}
}

func TestDelayWaitsOnTheClock(t *testing.T) {
	c := NewTestClock(start)
	s, err := NewSchedule(1, c, Rule{Op: "sync", Key: "*", Action: FaultAction{Kind: Delay, Delay: 5 * time.Second}})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan FaultAction)
	go func() { done <- s.At("sync", []byte("seg-1")) }()

	// Wait for At to set its timer, or Advance below could run first and
	// leave it waiting forever. An At that waited in real time, or not at
	// all, never sets one.
	for deadline := time.Now().Add(2 * time.Second); c.Pending() == 0; {
		if time.Now().After(deadline) {
			t.Fatal("At set no timer on the schedule's clock")
		}
		time.Sleep(time.Millisecond)
	}

	// The timer, not done: At woken early would need scheduling to show.
	c.Advance(4 * time.Second)
	if c.Pending() != 1 {
		t.Fatal("At's wait ended before its delay had elapsed on the clock")
	}

	c.Advance(time.Second)
	select {
	case a := <-done:
		if a.Kind != Delay {
			t.Fatalf("At returned %v, want the delay", a)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("At did not return once its delay had elapsed on the clock")
	}
}

func TestNewScheduleRefusesActionsASeamCannotApply(t *testing.T) {
	c := NewTestClock(start)
	cases := map[string]Rule{
		"fail with no error":  {Op: "*", Key: "*", Action: FaultAction{Kind: Fail}},
		"zero delay":          {Op: "*", Key: "*", Action: FaultAction{Kind: Delay}},
		"negative tear":       {Op: "*", Key: "*", Action: FaultAction{Kind: Tear, N: -1}},
		"probability above 1": {Op: "*", Key: "*", Prob: 1.5, Action: eio},
		"negative from":       {Op: "*", Key: "*", From: -1, Action: eio},
		"negative count":      {Op: "*", Key: "*", Count: -1, Action: eio},
		"unknown kind":        {Op: "*", Key: "*", Action: FaultAction{Kind: Reorder + 1}},
		"empty op":            {Op: "", Key: "*", Action: eio},
		"empty key":           {Op: "*", Key: "", Action: eio},
	}
	for name, r := range cases {
		if _, err := NewSchedule(1, c, r); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	if _, err := NewSchedule(1, nil, Rule{Op: "*", Key: "*", Action: FaultAction{Kind: Delay, Delay: time.Second}}); err == nil {
		t.Error("a delay with no clock: accepted")
	}
}

// TestConcurrentAt guards against data races only when run under -race.
func TestConcurrentAt(t *testing.T) {
	s := newSchedule(t, 1, Rule{Op: "read", Key: "*", Count: 10, Action: eio})

	var mu sync.Mutex
	failed := 0

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				if s.At("read", []byte("seg-1")) == eio {
					mu.Lock()
					failed++
					mu.Unlock()
				}
			}
		})
	}
	wg.Wait()

	if failed != 10 {
		t.Fatalf("Count 10 failed %d of 800 concurrent calls", failed)
	}
}
