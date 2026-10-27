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
// real time it spends running them.
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
)

// shrink returns a sequence that still fails, as short as the search finds:
// steps removed, then each kept step's extent shortened, while fails holds.
// steps must fail.
func shrink(steps []Step, fails func([]Step) bool) []Step {
	attempts, deadline := 0, time.Now().Add(shrinkTime)
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

// openError is a target that could not be opened, which says nothing about a
// sequence. It reads as the target's own error, so a fixture's failure keeps
// the prefix that tells it from the check's.
type openError struct{ err error }

func (e openError) Error() string { return e.err.Error() }
func (e openError) Unwrap() error { return e.err }

// Check runs steps against a journal newTarget opens, which holds nothing,
// and reports to tb what goes wrong. A divergence is shrunk, each candidate
// run on a journal of its own, and the shortest failing sequence is written
// to tb.ArtifactDir() as the corpus file name, with notes and the failure as
// its comments, ready to commit. A misuse of the model, or an error of the
// target, is reported as it is: it is no evidence about the journal, and
// shrinking would only hide it. A target that cannot be opened, or a journal
// call not written yet, ends the test, the latter as "stub: <call>".
// newTarget returns the target and the function that closes it.
func Check(tb testing.TB, name string, steps []Step, newTarget func() (Target, func(), error), notes ...string) {
	tb.Helper()

	skipped := 0
	run := func(c []Step) error {
		tgt, done, err := newTarget()
		if err != nil {
			return openError{err}
		}
		defer done()
		_, skipped, err = runCounting(tb.Context(), tgt, c)
		return err
	}

	err := run(steps)
	if skipped > 0 {
		tb.Logf("%s: %d crash and reopen steps did nothing: a file held more settled markers than a crash may leave in doubt", name, skipped)
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

	shrunk := shrink(steps, func(c []Step) bool { return errors.Is(run(c), ErrDiverged) })
	again := run(shrunk)
	if !errors.Is(again, ErrDiverged) {
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
