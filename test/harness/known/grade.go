package known

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/marmos91/dittofs/test/harness/report"
)

// Grade grades each result of one cell in place, setting its grade and the
// status the grade gives it. entries are the suite's known file's, then its
// quarantine list's (see Quarantine.Entry); only those that apply where the
// cell runs, at, count. listed says the suite has a known file, so its skips
// are graded rather than reported ungraded. A pass is unexpected when its
// entry, of either kind, isn't intermittent and every result in the cell it
// matches passed: a pattern one of whose tests still fails, or a name with
// one failing result among several, still says what the server does.
func Grade(tests []report.Test, listed bool, entries []Entry, at map[string]string) {
	var apply []Entry
	for _, e := range entries {
		if e.applies(at) {
			apply = append(apply, e)
		}
	}
	allPassed := make([]bool, len(apply))
	for i, e := range apply {
		allPassed[i] = !slices.ContainsFunc(tests, func(t report.Test) bool { return t.Status != report.Passed && e.matches(t.Name) })
	}
	throughSubtests := make([]bool, len(tests))
	for i := range tests {
		throughSubtests[i] = failedThroughSubtests(tests, i, apply)
	}
	for i := range tests {
		t := &tests[i]
		// decision: a result its reader graded keeps that grade: a crash or
		// a hang, which no entry may excuse, or a reader's own grade, as a
		// tool that grades its results itself gives.
		if t.Grade != "" {
			continue
		}
		switch t.Status {
		case report.Passed:
			if e := entryFor(apply, t.Name, ""); e >= 0 && apply[e].quarantined {
				t.Grade, t.Reason, t.Issue = report.Quarantined, apply[e].Reason, apply[e].Issue
			} else if e >= 0 && !apply[e].Intermittent && allPassed[e] {
				t.Status, t.RawStatus, t.Grade = report.Failed, report.Passed, report.UnexpectedPass
				t.Reason, t.Issue = apply[e].Reason, apply[e].Issue
			}
		case report.Failed:
			t.Grade = report.New
			switch e := entryFor(apply, t.Name, "fail"); {
			case e >= 0 && apply[e].covers(t.Message):
				t.Status, t.RawStatus, t.Grade = report.Skipped, report.Failed, report.Known
				if apply[e].quarantined {
					t.Grade = report.Quarantined
				}
				t.Reason, t.Issue = apply[e].Reason, apply[e].Issue
			case throughSubtests[i]:
				t.Status, t.RawStatus, t.Grade = report.Skipped, report.Failed, report.Known
				t.Reason = "only its subtests failed, and they are known failures"
			case e >= 0:
				t.Reason = notCovered(apply[e])
			}
		case report.Skipped:
			switch e := entryFor(apply, t.Name, "skip"); {
			case !listed:
				t.Grade = report.Ungraded
			case e >= 0 && apply[e].covers(t.Message):
				t.Grade, t.Reason, t.Issue = report.ExpectedSkip, apply[e].Reason, apply[e].Issue
			default:
				if e >= 0 {
					t.Reason = notCovered(apply[e])
				}
				// decision: rawStatus is set here too, since grading changed
				// the status; it is set whenever a grade changes the status,
				// unless the reader kept the tool's own, as TRX's NotExecuted.
				t.Status, t.RawStatus, t.Grade = report.Other, cmp.Or(t.RawStatus, report.Skipped), report.UnexpectedSkip
			}
		default:
			t.Grade = report.Incomplete
		}
	}
}

// failedThroughSubtests reports whether the failed result at i failed only
// because of its subtests: results in the same suite named under its name,
// and "/", that failed too. It needs at least one, and each must be covered:
// a known-failure entry matches it, its reader graded it known, or it has
// failed subtests of its own, which are among those checked. A subtest its
// reader graded otherwise, such as a crash or a hang, is never covered.
//
// decision: a parent with no entry of its own is graded from its subtests,
// because go test fails every parent of a failed subtest, so an entry naming
// one subtest would otherwise leave its parents failing. go test reports a
// parent that also failed on its own assertion the same way, so that failure
// is excused with them. Refuse subtest entries instead, and let only a
// parent's entry (which matches all its subtests) excuse them, if a parent's
// own failure is ever hidden this way. Other tools' names that hold a "/",
// such as pjdfstest's, have no result at the bare prefix, so it applies to Go
// tests alone.
func failedThroughSubtests(tests []report.Test, i int, apply []Entry) bool {
	if tests[i].Status != report.Failed || tests[i].Grade != "" {
		return false
	}
	// failedUnder reports whether a result failed under the one at p.
	failedUnder := func(p int) func(report.Test) bool {
		return func(s report.Test) bool {
			return s.Status == report.Failed && slices.Equal(s.Suite, tests[p].Suite) && strings.HasPrefix(s.Name, tests[p].Name+"/")
		}
	}
	found := false
	for j, s := range tests {
		if !failedUnder(i)(s) {
			continue
		}
		found = true
		switch {
		case s.Grade != "":
			if s.Grade != report.Known {
				return false
			}
		case describes(apply, s, "fail"):
		case !slices.ContainsFunc(tests, failedUnder(j)):
			return false
		}
	}
	return found
}

// describes reports whether the entry of status that entryFor picks for the
// result t describes it: it matches t's name, and covers t's message.
func describes(entries []Entry, t report.Test, status string) bool {
	e := entryFor(entries, t.Name, status)
	return e >= 0 && entries[e].covers(t.Message)
}

// notCovered is the reason of a result its entry matches by name but not by
// message: it is graded as if it had no entry, and the reason says why.
func notCovered(e Entry) string {
	return fmt.Sprintf("known.yaml lists it, but only with a message containing %q", e.Message)
}

// entryFor returns the index of the entry that grades the result named name,
// of status fail or skip, or of either when status is "", or -1 when none
// matches.
//
// decision: when several entries match, one naming the test exactly grades
// it, as the shell's kf_reason prefers one, and otherwise the first in the
// file does. The shell then takes whichever pattern its hash table gives
// first, which no list relies on; file order is the same on every run.
func entryFor(entries []Entry, name, status string) int {
	first := -1
	for i, e := range entries {
		if status != "" && e.Status != status || !e.matches(name) {
			continue
		}
		if e.Test == name {
			return i
		}
		if first < 0 {
			first = i
		}
	}
	return first
}
