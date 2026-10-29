package sim

import (
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"math/rand/v2"
	"sync"
	"time"
)

// Waiter is the part of a clock a Schedule's Delay waits on, so any clock with
// this After can drive one, not only a TestClock.
type Waiter interface {
	After(d time.Duration) <-chan struct{}
}

var (
	_ Waiter = (*TestClock)(nil)
	_ Fault  = (*Schedule)(nil)
)

// Rule selects some of a seam's calls and the action to take on them.
//
// A call matches a rule when its op matches Op and its key matches Key. Every
// call that matches is counted, from 1, whether or not the rule fires on it.
// The rule fires on the matching calls numbered From to From+Count-1, and on
// each of those only with probability Prob when Prob is set.
type Rule struct {
	Op  string // a Match pattern over the op name; "*" matches every op
	Key string // a Match pattern over the key; "*" matches every key, "" included

	From  int     // the first matching call the rule fires on; 0 means 1
	Count int     // how many calls, from From, it fires on; 0 means every one
	Prob  float64 // in (0, 1]: fire with this probability; 0 means always

	Action FaultAction
}

// Schedule is a Fault driven by a seed and a list of rules.
//
// For each call, every rule that matches counts it, and each rule that sets
// Prob draws for it from its own stream, seeded from the seed and a hash of
// the rule's fields. The first rule in the list that fires decides the
// action; when none fires, the call passes. The same seed and rules therefore
// give the same actions over the same sequence of calls, and one rule's draws
// do not change when another rule is added, removed or moved. Its methods
// are safe for concurrent use; concurrent calls are numbered in the order At
// is entered.
type Schedule struct {
	clock Waiter

	rules []scheduled
	mu    sync.Mutex
}

// scheduled is one rule and its state.
type scheduled struct {
	Rule
	calls int // matching calls seen
	rng   *rand.Rand
}

// NewSchedule returns a schedule of rules seeded with seed, whose delays wait
// on clock. clock may be nil when no rule delays. It refuses an empty Op or
// Key, a negative From or Count, a Prob outside [0, 1], a negative Delay, and
// a loss with an Err.
func NewSchedule(seed uint64, clock Waiter, rules ...Rule) (*Schedule, error) {
	s := &Schedule{clock: clock}

	for i, r := range rules {
		if err := r.validate(clock); err != nil {
			return nil, fmt.Errorf("sim: rule %d: %w", i, err)
		}

		s.rules = append(s.rules, scheduled{
			Rule: r,
			rng:  rand.New(rand.NewPCG(seed, r.hash())),
		})
	}

	return s, nil
}

func (r Rule) validate(clock Waiter) error {
	a := r.Action
	switch {
	case r.Op == "" || r.Key == "":
		return errors.New(`empty Op or Key: "*" matches every one, the empty key included`)
	case r.From < 0 || r.Count < 0:
		return errors.New("negative From or Count")
	case !(r.Prob >= 0 && r.Prob <= 1):
		return fmt.Errorf("a Prob of %v, outside [0, 1]", r.Prob)
	case a.Delay < 0:
		return fmt.Errorf("delay of %v", a.Delay)
	case a.Delay > 0 && clock == nil:
		return errors.New("delay with no clock")
	case a.TearAfter < 0 && a.Err != nil:
		return errors.New("a loss with an Err: a lost operation reports success")
	}

	return nil
}

// hash identifies the rule by its content, so its stream does not depend on
// where it sits in the list. Err is taken by its message.
func (r Rule) hash() uint64 {
	var msg string
	if r.Action.Err != nil {
		msg = r.Action.Err.Error()
	}

	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%q %q %d %d %x %q %d %d",
		r.Op, r.Key, r.From, r.Count, math.Float64bits(r.Prob),
		msg, r.Action.Delay, r.Action.TearAfter)
	return h.Sum64()
}

// At returns the results for one call of op on key. A delay it first waits
// out on the schedule's clock, without holding the schedule, so other calls
// are decided meanwhile.
//
// decision: At waits the delay itself, and returns it only to say so, because
// the seams it drives take no clock of their own; a seam applies a returned
// delay as already elapsed.
//
// decision: err sits between delay and tearAfter, against Go's convention,
// because the results are RFC 8's Fault signature, which every seam declares
// in its own package and a Schedule must satisfy as is.
func (s *Schedule) At(op string, key []byte) (delay time.Duration, err error, tearAfter int) { //nolint:staticcheck // ST1008: the order is Fault's, see above
	a := s.decide(op, string(key))
	if a.Delay > 0 {
		<-s.clock.After(a.Delay)
	}

	return a.Delay, a.Err, a.TearAfter
}

// decide selects the FaultAction for a call of op on key according to the rules.
// It does not wait for any Delay; that is handled by At.
func (s *Schedule) decide(op, key string) FaultAction {
	s.mu.Lock()
	defer s.mu.Unlock()

	var chosen *FaultAction
	for i := range s.rules {
		r := &s.rules[i]
		if !Match(r.Op, op) || !Match(r.Key, key) {
			continue
		}

		r.calls++
		fires := r.calls >= max(r.From, 1) && (r.Count == 0 || r.calls < max(r.From, 1)+r.Count)

		// Drawn on every matching call, so the stream advances with the calls
		// the rule sees, not with the ones it fires on.
		if r.Prob > 0 && r.rng.Float64() >= r.Prob {
			fires = false
		}

		if fires && chosen == nil {
			chosen = &r.Action
		}
	}

	if chosen == nil {
		return FaultAction{}
	}

	return *chosen
}

// Match reports whether name matches pattern, byte by byte: a '*' in pattern
// matches any run of bytes, '/' and bytes that are not valid UTF-8 included,
// and every other byte matches only itself. So "*" matches every name, the
// empty one included, and "shard/*" every name that starts with "shard/".
func Match(pattern, name string) bool {
	// On a mismatch, the latest '*' takes one more byte and the rest of the
	// pattern is tried again from there.
	p, n := 0, 0
	star, from := -1, 0
	for n < len(name) {
		switch {
		case p < len(pattern) && pattern[p] == '*':
			star, from = p, n
			p++
		case p < len(pattern) && pattern[p] == name[n]:
			p++
			n++
		case star >= 0:
			from++
			p, n = star+1, from
		default:
			return false
		}
	}

	for p < len(pattern) && pattern[p] == '*' {
		p++
	}

	return p == len(pattern)
}
