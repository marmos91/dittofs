package known

import (
	"slices"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/test/harness/report"
)

// results are a cell's results, each written as "name status", or
// "name status grade" for one its reader already graded.
func results(lines ...string) []report.Test {
	var out []report.Test
	for _, l := range lines {
		f := strings.Fields(l)
		t := report.Test{Name: f[0], Status: f[1]}
		if len(f) > 2 {
			t.Grade = f[2]
		}
		out = append(out, t)
	}
	return out
}

// shown is each result as "name: status (the tool's status) [grade] reason
// issue", leaving out what it doesn't have.
func shown(tests []report.Test) []string {
	var out []string
	for _, t := range tests {
		line := t.Name + ": " + t.Status
		if t.RawStatus != "" {
			line += " (" + t.RawStatus + ")"
		}
		if t.Grade != "" {
			line += " [" + t.Grade + "]"
		}
		out = append(out, strings.TrimSpace(line+" "+strings.TrimSpace(t.Reason+" "+t.Issue)))
	}
	return out
}

// noFile stands for a suite without a known file.
const noFile = "no file"

// throughSubtests is the reason a parent graded from its subtests gets.
const throughSubtests = "only its subtests failed, and they are known failures"

// One case per rule of the grades table and of the unexpected-pass rule:
// how each result is graded against the entries that apply where its cell
// runs, and the status its grade gives it.
func TestGrade(t *testing.T) {
	const wontfix = "fix: wontfix, category: proto"
	for _, c := range []struct {
		name    string
		known   string // the suite's known file, or noFile
		at      map[string]string
		results []report.Test
		want    []string
	}{
		{"a pass no entry matches has no grade", "", nil,
			results("a passed"), []string{"a: passed"}},
		{"a failure no entry matches is new", "", nil,
			results("a failed"), []string{"a: failed [new]"}},
		{"a failure a known-failure entry matches is known, with the entry's reason and issue",
			"- { test: a, status: fail, fix: planned, issue: '#12', category: bug, reason: slow flush }\n", nil,
			results("a failed"), []string{"a: skipped (failed) [known] slow flush #12"}},
		{"an expected skip that runs and fails is a new failure",
			"- { test: a, status: skip, " + wontfix + ", reason: needs root }\n", nil,
			results("a failed"), []string{"a: failed [new]"}},
		{"unless a known-failure entry covers it too",
			"- { test: a, status: skip, " + wontfix + ", reason: needs root }\n- { test: a, status: fail, " + wontfix + ", reason: no locks }\n", nil,
			results("a failed"), []string{"a: skipped (failed) [known] no locks"}},
		{"a known failure that passes is an unexpected pass",
			"- { test: a, status: fail, " + wontfix + ", reason: no locks }\n", nil,
			results("a passed"), []string{"a: failed (passed) [unexpected_pass] no locks"}},
		{"an expected skip that runs and passes is an unexpected pass",
			"- { test: a, status: skip, " + wontfix + ", reason: needs root }\n", nil,
			results("a passed"), []string{"a: failed (passed) [unexpected_pass] needs root"}},
		{"an intermittent entry's pass is reported, not failed",
			"- { test: a, status: fail, " + wontfix + ", reason: races, intermittent: true }\n", nil,
			results("a passed"), []string{"a: passed"}},
		{"a pattern one of whose tests still fails stays known",
			"- { test: smb2.lock.*, status: fail, " + wontfix + ", reason: no locks }\n", nil,
			results("smb2.lock.rw failed", "smb2.locking passed"),
			[]string{"smb2.lock.rw: skipped (failed) [known] no locks", "smb2.locking: passed"}},
		{"a pass is judged on its own entry's results, a skip among them",
			"- { test: a*, status: fail, " + wontfix + ", reason: r }\n- { test: b, status: fail, " + wontfix + ", reason: r }\n", nil,
			results("a1 skipped", "a2 passed", "b passed", "c failed"),
			[]string{"a1: other (skipped) [unexpected_skip]", "a2: passed", "b: failed (passed) [unexpected_pass] r", "c: failed [new]"}},
		{"a skip an expected-skip entry matches is expected, with the entry's reason",
			"- { test: a, status: skip, " + wontfix + ", reason: needs root }\n", nil,
			results("a skipped"), []string{"a: skipped [expected_skip] needs root"}},
		{"a skip no entry lists, in a suite with a known file, is unexpected", "", nil,
			results("a skipped"), []string{"a: other (skipped) [unexpected_skip]"}},
		{"a known-failure entry doesn't make a skip expected",
			"- { test: a, status: fail, " + wontfix + ", reason: no locks }\n", nil,
			results("a skipped"), []string{"a: other (skipped) [unexpected_skip]"}},
		{"a skip in a suite without a known file is ungraded", noFile, nil,
			results("a skipped"), []string{"a: skipped [ungraded]"}},
		{"a result whose output stopped is incomplete, whatever the entries say",
			"- { test: a, status: fail, " + wontfix + ", reason: no locks }\n- { test: a, status: skip, " + wontfix + ", reason: needs root }\n", nil,
			results("a other"), []string{"a: other [incomplete]"}},
		{"a crash or a hang keeps its grade, and still counts against its entry",
			"- { test: a*, status: fail, " + wontfix + ", reason: no locks }\n", nil,
			results("a1 failed new", "a2 passed"), []string{"a1: failed [new]", "a2: passed"}},
		{"a parent that failed only through known subtests is known, at every level",
			"- { test: TestP/a/b, status: fail, " + wontfix + ", reason: no locks }\n", nil,
			results("TestP/a/b failed", "TestP/a failed", "TestP failed"),
			[]string{"TestP/a/b: skipped (failed) [known] no locks", "TestP/a: skipped (failed) [known] " + throughSubtests, "TestP: skipped (failed) [known] " + throughSubtests}},
		{"a parent of a subtest that crashed stays new",
			"- { test: TestP/a, status: fail, " + wontfix + ", reason: no locks }\n", nil,
			results("TestP/a failed new", "TestP failed"), []string{"TestP/a: failed [new]", "TestP: failed [new]"}},
		{"a subtest in another package doesn't excuse a test of the same name",
			"- { test: TestP/a, status: fail, " + wontfix + ", reason: no locks }\n", nil,
			[]report.Test{{Name: "TestP/a", Suite: []string{"q"}, Status: report.Failed}, {Name: "TestP", Suite: []string{"p"}, Status: report.Failed}},
			[]string{"TestP/a: skipped (failed) [known] no locks", "TestP: failed [new]"}},
		{"an entry doesn't apply where the cell has no value for a key it names",
			"- { test: a, status: fail, " + wontfix + ", reason: badger only, where: { profile: [badger] } }\n", map[string]string{"nfs": "3"},
			results("a failed"), []string{"a: failed [new]"}},
		{"a failure whose message contains the entry's message is known",
			"- { test: a, status: fail, " + wontfix + ", reason: no journal yet, message: 'fixture:' }\n", nil,
			[]report.Test{{Name: "a", Status: report.Failed, Message: "helpers_test.go:12: fixture: Init: not implemented"}},
			[]string{"a: skipped (failed) [known] no journal yet"}},
		{"a failure with another message is new, and its reason says what the entry expects",
			"- { test: a, status: fail, " + wontfix + ", reason: no journal yet, message: 'fixture:' }\n", nil,
			[]report.Test{{Name: "a", Status: report.Failed, Message: "rfc1_a_test.go:40: read 0 bytes, want 4096"}},
			[]string{`a: failed [new] known.yaml lists it, but only with a message containing "fixture:"`}},
		{"a skip with another message than its entry's is unexpected",
			"- { test: a, status: skip, " + wontfix + ", reason: needs root, message: 'needs root' }\n", nil,
			[]report.Test{{Name: "a", Status: report.Skipped, Message: "no kernel client"}},
			[]string{`a: other (skipped) [unexpected_skip] known.yaml lists it, but only with a message containing "needs root"`}},
		{"a parent whose subtest fails with another message than its entry's stays new",
			"- { test: TestP/a, status: fail, " + wontfix + ", reason: stub, message: 'stub:' }\n", nil,
			[]report.Test{{Name: "TestP/a", Status: report.Failed, Message: "want 1, got 2"}, {Name: "TestP", Status: report.Failed}},
			[]string{`TestP/a: failed [new] known.yaml lists it, but only with a message containing "stub:"`, "TestP: failed [new]"}},
		{"a parent whose entry's message only its subtests' failures contain is known through them",
			"- { test: TestP, status: fail, " + wontfix + ", reason: stub, message: 'stub:' }\n", nil,
			[]report.Test{{Name: "TestP/a", Status: report.Failed, Message: "x_test.go:9: stub: not implemented"}, {Name: "TestP", Status: report.Failed}},
			[]string{"TestP/a: skipped (failed) [known] stub", "TestP: skipped (failed) [known] " + throughSubtests}},
		{"an entry naming the test grades it before a pattern, and otherwise the first in the file",
			"- { test: smb2.lock.*, status: fail, " + wontfix + ", reason: lock }\n- { test: smb2.lock.rw, status: fail, " + wontfix + ", reason: rw }\n- { test: smb2.*, status: fail, " + wontfix + ", reason: smb2 }\n", nil,
			results("smb2.lock.rw failed", "smb2.lock.x failed"),
			[]string{"smb2.lock.rw: skipped (failed) [known] rw", "smb2.lock.x: skipped (failed) [known] lock"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var entries []Entry
			if c.known != noFile {
				var err error
				if entries, err = Parse("known.yaml", []byte(c.known)); err != nil {
					t.Fatal(err)
				}
			}
			Grade(c.results, c.known != noFile, entries, c.at)
			if got := shown(c.results); !slices.Equal(got, c.want) {
				t.Errorf("got  %q\nwant %q", got, c.want)
			}
		})
	}
}

// A failure's entry is intermittent when the entry Grade picks for it, among
// those that apply where its cell runs, is: not one that applies elsewhere,
// nor a pattern when an entry names the test.
func TestIntermittent(t *testing.T) {
	const fail = "status: fail, fix: wontfix, category: env, reason: r"
	entries, err := Parse("known.yaml", []byte("- { test: 'a*', "+fail+", where: { nfs: ['4.1'] }, intermittent: true }\n- { test: a, "+fail+" }\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		test, nfs string
		want      bool
	}{
		{"a1", "4.1", true},
		{"a1", "3", false},
		{"a", "4.1", false},
	} {
		if got := Intermittent(entries, c.test, map[string]string{"nfs": c.nfs}); got != c.want {
			t.Errorf("%s at nfs %s: %v, want %v", c.test, c.nfs, got, c.want)
		}
	}
}
