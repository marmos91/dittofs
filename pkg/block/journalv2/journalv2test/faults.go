package journalv2test

import "github.com/marmos91/dittofs/pkg/sim"

// Rule selects some of the seam's calls and what the seam does with them: the
// calls of an op on a file whose name matches a key pattern, as sim.Match
// takes it, counted from the first. Failing, Dropping and Tearing build one.
type Rule = sim.Rule

// Failing returns a rule that fails the first count calls of op on a file
// matching key with err, or every such call when count is 0.
func Failing(op, key string, count int, err error) Rule {
	return Rule{Op: op, Key: key, Count: count, Action: sim.FaultAction{Kind: sim.Fail, Err: err}}
}

// Dropping returns a rule that drops the first count calls of op on a file
// matching key, or every such call when count is 0: each reports success
// and does not become durable, as SetFault describes for the op.
func Dropping(op, key string, count int) Rule {
	return Rule{Op: op, Key: key, Count: count, Action: sim.FaultAction{Kind: sim.Drop}}
}

// Tearing returns a rule that tears the first count calls of op on a file
// matching key after their first n bytes, or every such call when count is
// 0: with err nil the write is reported whole and only its first n bytes can
// become durable; with err set only those bytes land, and the write returns
// n and err.
func Tearing(op, key string, count int, n int64, err error) Rule {
	return Rule{Op: op, Key: key, Count: count, Action: sim.FaultAction{Kind: sim.Tear, N: n, Err: err}}
}

// Schedule sets rules as the seam's fault, replacing any set before: for
// each call, the first rule that fires decides what the seam does, and a
// call no rule fires on passes. It returns the schedule's refusal of a rule,
// such as an empty op or key or a fail with a nil error, and then leaves the
// seam's fault as it was.
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
