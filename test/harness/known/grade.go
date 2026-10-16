package known

import (
	"slices"

	"github.com/marmos91/dittofs/test/harness/report"
)

// Grade grades each result of one cell in place, setting its grade and the
// status the grade gives it. entries are the suite's known file's; only those
// that apply where the cell runs, at, count. listed says the suite has a
// known file, so its skips are graded rather than reported ungraded. A
// result already graded keeps its grade: one its reader graded itself, or a
// crash or a hang, which no entry excuses.
//
//   - A failure is known when a known-failure entry matches it, else new.
//   - A skip is an expected skip when an expected-skip entry matches it, else
//     an unexpected one; in a suite without a known file it's ungraded.
//   - A test whose output stopped before its result is incomplete, whatever
//     the entries say.
//   - A pass is unexpected when the entry that matches it, of either kind,
//     isn't intermittent and every result in the cell it matches passed: a
//     pattern one of whose tests still fails, or a name with one failing
//     result among several, still says what the server does.
func Grade(tests []report.Test, listed bool, entries []Entry, at map[string]string) {
	var apply []Entry
	for _, e := range entries {
		if e.Applies(at) {
			apply = append(apply, e)
		}
	}
	allPassed := make([]bool, len(apply))
	for i, e := range apply {
		allPassed[i] = !slices.ContainsFunc(tests, func(t report.Test) bool { return t.Status != report.Passed && Match(e.Test, t.Name) })
	}
	for i := range tests {
		t := &tests[i]
		if t.Grade != "" {
			continue
		}
		switch t.Status {
		case report.Passed:
			if e := entryFor(apply, t.Name, ""); e >= 0 && !apply[e].Intermittent && allPassed[e] {
				t.Status, t.RawStatus, t.Grade = report.Failed, report.Passed, report.UnexpectedPass
				t.Reason, t.Issue = apply[e].Reason, apply[e].Issue
			}
		case report.Failed:
			t.Grade = report.New
			if e := entryFor(apply, t.Name, "fail"); e >= 0 {
				t.Status, t.RawStatus, t.Grade = report.Skipped, report.Failed, report.Known
				t.Reason, t.Issue = apply[e].Reason, apply[e].Issue
			}
		case report.Skipped:
			switch e := entryFor(apply, t.Name, "skip"); {
			case !listed:
				t.Grade = report.Ungraded
			case e >= 0:
				t.Grade, t.Reason, t.Issue = report.ExpectedSkip, apply[e].Reason, apply[e].Issue
			default:
				t.Status, t.RawStatus, t.Grade = report.Other, report.Skipped, report.UnexpectedSkip
			}
		default:
			t.Grade = report.Incomplete
		}
	}
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
		if status != "" && e.Status != status || !Match(e.Test, name) {
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
