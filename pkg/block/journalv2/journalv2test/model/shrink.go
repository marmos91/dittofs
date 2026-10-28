package model

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// shrinkAttempts and shrinkTime bound the sequences one shrink runs, and the
// real time it spends running them; the test's own deadline, less
// shrinkMargin, bounds that time too, so a test that checks several sequences
// shares one budget and never reaches its timeout mid-shrink.
//
// ponytail: shrinking removes ever smaller chunks of the sequence until no
// removal keeps it failing, then halves each kept extent, running the whole
// candidate from a fresh journal every time: O(n log n) runs for n steps
// per pass, each O(n) steps long, and as many passes as keep removing steps,
// so O(n²) runs at worst. On a real journal one run can take seconds, more
// when calls wait for spare segments, so the caps keep a failure from
// outlasting the test's timeout; past either, the shrinker returns the
// shortest failing sequence found so far. Shrink with a smarter search, such as dropping the
// steps after the failing one first, if a real journal's failures hit them.
const (
	shrinkAttempts = 2000
	shrinkTime     = 5 * time.Minute

	// shrinkMargin is how long before the test's deadline shrinking stops,
	// leaving time to run the shrunk sequence again, write it, and run the
	// test's other sequences.
	shrinkMargin = 2 * time.Minute
)

// shrink returns a sequence that still fails, as short as the search finds
// by deadline: steps removed, then each kept step's extent shortened, while
// fails holds. steps must fail.
func shrink(steps []Step, fails func([]Step) bool, deadline time.Time) []Step {
	attempts := 0
	try := func(c []Step) bool {
		if attempts >= shrinkAttempts || time.Now().After(deadline) {
			return false
		}
		attempts++
		return fails(c)
	}

	cur := slices.Clone(steps)
	for removed := true; removed; {
		removed = false
		for chunk := max(len(cur)/2, 1); chunk >= 1; chunk /= 2 {
			for i := 0; i < len(cur); {
				c := slices.Concat(cur[:i], cur[min(i+chunk, len(cur)):])
				if try(c) {
					cur, removed = c, true
					continue
				}
				i += chunk
			}
		}
	}
	for i := range cur {
		for cur[i].Len > 1 {
			c := slices.Clone(cur)
			c[i].Len /= 2
			if !try(c) {
				break
			}
			cur = c
		}
	}
	return cur
}

// shrinkDeadline is when a shrink starting now must stop: shrinkTime from
// now, or shrinkMargin before the test's deadline if that is sooner.
func shrinkDeadline(tb testing.TB) time.Time {
	deadline := time.Now().Add(shrinkTime)
	if d, ok := tb.(interface{ Deadline() (time.Time, bool) }); ok {
		if end, ok := d.Deadline(); ok && end.Add(-shrinkMargin).Before(deadline) {
			deadline = end.Add(-shrinkMargin)
		}
	}
	return deadline
}

// divergence is what tells one divergence from another: the operation of the
// step it was found at, and the model check that found it.
//
// decision: two divergences the same check finds at steps of the same
// operation are taken for the same, so shrinking may still trade one for
// another that differs only in the bytes or versions it names. Comparing the
// messages instead would refuse every candidate that shortens an extent or
// moves a version. Tell them apart by more, such as the kind of mismatch the
// check reports, if a shrunk sequence is found showing another bug.
type divergence struct {
	op    Op
	check string
}

// divergenceOf returns the divergence err reports, found at step ran of steps.
func divergenceOf(steps []Step, ran int, err error) divergence {
	var d divergence
	if ran < len(steps) {
		d.op = steps[ran].Op
	}
	var c checkError
	if errors.As(err, &c) {
		d.check = c.op
	}
	return d
}

// openError is a target that could not be opened, which says nothing about a
// sequence. It reads as the target's own error, so a fixture's failure keeps
// the prefix that tells it from the check's.
type openError struct{ err error }

func (e openError) Error() string { return e.err.Error() }
func (e openError) Unwrap() error { return e.err }

// Check runs steps against a journal newTarget opens, which holds nothing,
// and reports to tb what goes wrong. A divergence is shrunk to a shorter
// sequence showing the same divergence, each candidate
// run on a journal of its own, and the shortest failing sequence is written
// to tb.ArtifactDir() as the corpus file name, with notes and the failure as
// its comments, ready to commit. A misuse of the model, or an error of the
// target, is reported as it is: it is no evidence about the journal, and
// shrinking would only hide it. A target that cannot be opened, or a journal
// call not written yet, ends the test, the latter as "stub: <call>".
// newTarget returns the target and the function that closes it.
func Check(tb testing.TB, name string, steps []Step, newTarget func() (Target, func(), error), notes ...string) {
	tb.Helper()

	var n idle
	var ran int
	run := func(c []Step) error {
		tgt, done, err := newTarget()
		if err != nil {
			return openError{err}
		}
		defer done()
		ran, n, err = runCounting(tb.Context(), tgt, c)
		return err
	}

	err := run(steps)
	if n.restarts > 0 {
		tb.Logf("%s: %d crash and reopen steps did nothing: a file held more settled markers than a crash may leave in doubt", name, n.restarts)
	}
	if n.fills > 0 {
		tb.Logf("%s: %d fill steps did nothing: nothing offloaded from the file was left to fill", name, n.fills)
	}
	var open openError
	var stub stubError
	switch {
	case err == nil:
		return
	case errors.As(err, &open):
		tb.Fatalf("%v; opening the journal for %s", open, name)
		return
	case errors.As(err, &stub):
		tb.Fatalf("%v; running %s: %v", stub, name, err)
		return
	case !errors.Is(err, ErrDiverged):
		tb.Errorf("%s: %v", name, err)
		return
	}

	want := divergenceOf(steps, ran, err)
	same := func(c []Step) (bool, error) {
		err := run(c)
		return errors.Is(err, ErrDiverged) && divergenceOf(c, ran, err) == want, err
	}
	shrunk := shrink(steps, func(c []Step) bool { ok, _ := same(c); return ok }, shrinkDeadline(tb))
	ok, again := same(shrunk)
	if !ok {
		// The failure did not come back on the shrunk sequence, so the
		// journal does not fail the same way every time: keep it all.
		shrunk, again = steps, err
	}
	text := formatCorpus(shrunk, append(slices.Clone(notes), fmt.Sprintf("diverged: %v", again))...)
	path := filepath.Join(tb.ArtifactDir(), name)
	if werr := os.WriteFile(path, text, 0o644); werr != nil {
		path = fmt.Sprintf("nowhere (%v)", werr)
	}
	tb.Errorf("%s: the journal diverged from the model: %v\nshrunk from %d to %d steps; commit this as testdata/model/%s (written to %s, which outlives the test only under go test -artifacts):\n%s", name, err, len(steps), len(shrunk), name, path, text)
}
