package journaltest

import "github.com/cubbit/dittofs/pkg/sim"

// Rule selects some of the seam's calls and what the seam does with them: the
// calls of an op on a file whose name matches a key pattern, as sim.Match
// takes it, counted from the first. Failing, Dropping and Tearing build one.
type Rule = sim.Rule

// Failing returns a rule that fails the first count calls of op on a file
// matching key with err, or every such call when count is 0. A nil err is an
// action that does nothing, which Schedule refuses.
func Failing(op, key string, count int, err error) Rule {
	return Rule{
		Op:    op,
		Key:   key,
		Count: count,
		Action: sim.FaultAction{
			Err: err,
		},
	}
}

// FailingFrom returns a rule that fails the matching calls numbered from to
// from+count-1, rather than the first count of them: a check that must let a
// rule's own first matching calls pass — a spare's header write among the
// writes of the segment name — arms it past them. A from below 1 is read as
// 1, the schedule's own reading of it. A count of 0 fails every matching
// call from from on. A nil err is an action that does nothing, which
// Schedule refuses.
func FailingFrom(op, key string, from, count int, err error) Rule {
	return Rule{
		Op:    op,
		Key:   key,
		From:  from,
		Count: count,
		Action: sim.FaultAction{
			Err: err,
		},
	}
}

// FailingRandomly returns a rule that fails each matching call with err only
// with probability p, drawn per call from the schedule's own stream: a stress
// check spreads its failures over a run of calls rather than taking the
// first count of them. A p outside (0, 1] is refused by Schedule, as a
// probability outside it names no rule. A nil err is an action that does
// nothing, which Schedule refuses.
func FailingRandomly(op, key string, p float64, err error) Rule {
	return Rule{
		Op:   op,
		Key:  key,
		Prob: p,
		Action: sim.FaultAction{
			Err: err,
		},
	}
}

// Dropping returns a rule that loses the first count calls of op on a file
// matching key, or every such call when count is 0: each reports success
// and does not become durable, as SetFault describes for the op.
func Dropping(op, key string, count int) Rule {
	return Rule{
		Op:    op,
		Key:   key,
		Count: count,
		Action: sim.FaultAction{
			TearAfter: sim.Lose,
		},
	}
}

// Tearing returns a rule that tears the first count calls of op on a file
// matching key after their first n bytes, or every such call when count is
// 0: with err nil the write is reported whole and only its first n bytes can
// become durable; with err set only those bytes land, and the write returns
// n and err. n must be positive: a fault reads a tear of 0 as no tear and a
// negative one as a loss.
func Tearing(op, key string, count int, n int, err error) Rule {
	return Rule{
		Op:    op,
		Key:   key,
		Count: count,
		Action: sim.FaultAction{
			TearAfter: n,
			Err:       err,
		},
	}
}

// Schedule sets rules as the seam's fault, replacing any set before: for
// each call, the first rule that fires decides what the seam does, and a
// call no rule fires on passes. It returns the schedule's refusal of a rule,
// such as an empty op or key or an action that does nothing, and then leaves
// the seam's fault as it was.
//
// decision: the schedule has seed 1 and no clock. None of the rules built
// here draws on a probability or delays, so the seed changes nothing, and
// sim.NewSchedule refuses a delay with no clock rather than letting it
// wait on nothing. Take the fixture's clock here once a rule that delays is
// added.
func (s *Seam) Schedule(rules ...Rule) error {
	fault, err := sim.NewSchedule(1, nil, rules...)
	if err != nil {
		return err
	}

	s.SetFault(fault)

	return nil
}
