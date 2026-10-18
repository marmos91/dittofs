package run

import (
	"context"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/test/harness/report"
)

// knownFile lists a known failure, one that has since been fixed, and an
// expected skip.
const knownFile = `- { test: known, status: fail, fix: wontfix, category: proto, reason: no locks }
- { test: fixed, status: fail, fix: planned, issue: "#1", category: bug, reason: slow flush }
- { test: skips, status: skip, fix: wontfix, category: env, reason: needs root }
`

// A cell's result comes from its graded results: a known failure and an
// expected skip pass; an unexpected pass fails the cell; and an unlisted
// skip, or a test whose output stopped, which no entry excuses, leaves it
// not run.
func TestCellFromGrades(t *testing.T) {
	readers["tap"] = func(r report.Ran) ([]report.Test, error) {
		var tests []report.Test
		for _, line := range strings.Fields(string(r.Output)) {
			status, name, _ := strings.Cut(line, ":")
			tests = append(tests, report.Test{Name: name, Status: status})
		}
		return tests, nil
	}
	t.Cleanup(func() { delete(readers, "tap") })
	root, r := repo(t, `groups: { g: [green, fixed, unlisted, stopped] }
suites:
  green: { cmd: "cat {test}", tests: "green/*", format: tap, known: k.yaml }
  fixed: { cmd: "cat {test}", tests: "fixed/*", format: tap, known: k.yaml }
  unlisted: { cmd: "cat {test}", tests: "unlisted/*", format: tap, known: k.yaml }
  stopped: { cmd: "cat {test}", tests: "stopped/*", format: tap, known: k.yaml }
`, map[string]string{
		"k.yaml":     knownFile,
		"green/t":    "failed:known skipped:skips passed:other",
		"fixed/t":    "passed:fixed",
		"unlisted/t": "skipped:nope",
		"stopped/t":  "other:known",
	})
	rep := runIn(t, root, r, "g")
	want := []Outcome{
		{Cell: "green", Result: Passed},
		{Cell: "fixed", Result: Failed, Note: "1 passed unexpectedly: fixed"},
		{Cell: "unlisted", Result: NotRun, Note: "1 skipped unexpectedly: nope"},
		{Cell: "stopped", Result: NotRun, Note: "1 incomplete: known"},
	}
	if len(rep.Outcomes) != len(want) {
		t.Fatalf("outcomes %+v", rep.Outcomes)
	}
	for i, o := range rep.Outcomes {
		if o.Result != want[i].Result || o.Note != want[i].Note {
			t.Errorf("%s: %s %q, want %s %q", o.Cell, o.Result, o.Note, want[i].Result, want[i].Note)
		}
	}
	if rep.Status() != 1 {
		t.Errorf("status %d, want 1", rep.Status())
	}
}

// An exit suite's tests are graded too: a known failure passes its cell, but
// a test that timed out failed whatever the known file says, since a hang is
// a regression until shown otherwise. An entry applies only to the cells its
// where names, by matrix value, tier, option and OS.
func TestExitSuiteGraded(t *testing.T) {
	here := map[string]string{"darwin": "macos"}[runtime.GOOS]
	if here == "" {
		here = runtime.GOOS
	}
	root, r := repo(t, `suites:
  s:
    cmd: "sh {test} {v}"
    tests: "t/*.sh"
    matrix: { v: [a, b] }
    timeout: { test: 1s }
    known: k.yaml
    options:
      o: { cmd: "sh {test} {v}", matrix: { v: [c] } }
`, map[string]string{
		"t/fails.sh": "exit 1\n",
		"t/hangs.sh": "[ \"$1\" = a ] && sleep 30\nexit 0\n",
		"k.yaml": `- { test: t/fails.sh, status: fail, fix: wontfix, category: proto, reason: r, where: { v: [a] } }
- { test: t/fails.sh, status: fail, fix: wontfix, category: proto, reason: r, where: { tier: [pr], option: [o], os: [` + here + `] } }
- { test: t/hangs.sh, status: fail, fix: wontfix, category: proto, reason: r, where: { v: [a] } }
`,
	})
	cells, err := Select(root, r, "s", "", "pr", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	option, err := Select(root, r, "s", "o", "pr", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Run(context.Background(), append(cells, option...), Options{Root: root, Results: filepath.Join(root, "results"), Out: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	want := []Outcome{
		{Cell: "s/a", Result: Failed, Note: "t/hangs.sh: timed out after 1s"},
		{Cell: "s/b", Result: Failed, Note: "t/fails.sh: `sh t/fails.sh b` exited 1"},
		{Cell: "s/o/c", Result: Passed},
	}
	if len(rep.Outcomes) != len(want) {
		t.Fatalf("outcomes %+v", rep.Outcomes)
	}
	for i, o := range rep.Outcomes {
		if o.Cell != want[i].Cell || o.Result != want[i].Result || o.Note != want[i].Note {
			t.Errorf("got %s %s %q, want %s %s %q", o.Cell, o.Result, o.Note, want[i].Cell, want[i].Result, want[i].Note)
		}
	}
}

// A go-json suite with a known file grades its results: a known failure
// passes the cell, counted apart in its note; a known failure that passes
// fails it; a skip it doesn't list leaves the cell not run; and a test its
// binary crashed in failed, which no entry excuses.
func TestGoJSONGraded(t *testing.T) {
	for _, c := range []struct {
		name, events, code string
		result, note       string
	}{
		{"a known failure", `{"Action":"run","Package":"p","Test":"TestKnown"}
{"Action":"fail","Package":"p","Test":"TestKnown"}
{"Action":"run","Package":"p","Test":"TestPass"}
{"Action":"pass","Package":"p","Test":"TestPass"}
{"Action":"fail","Package":"p"}
`, "1", Passed, "1 passed, 0 failed, 0 skipped, 1 known"},
		{"an unexpected pass", `{"Action":"run","Package":"p","Test":"TestKnown"}
{"Action":"pass","Package":"p","Test":"TestKnown"}
{"Action":"pass","Package":"p"}
`, "0", Failed, "1 passed, 0 failed, 0 skipped; 1 passed unexpectedly: TestKnown"},
		{"an unlisted skip", `{"Action":"run","Package":"p","Test":"TestSkip"}
{"Action":"skip","Package":"p","Test":"TestSkip"}
{"Action":"pass","Package":"p"}
`, "0", NotRun, "0 passed, 0 failed, 1 skipped; 1 skipped unexpectedly: TestSkip"},
		{"a crash", `{"Action":"run","Package":"p","Test":"TestKnown"}
{"Action":"output","Package":"p","Test":"TestKnown","Output":"panic: boom\n"}
{"Action":"fail","Package":"p"}
`, "1", Failed, "0 passed, 1 failed, 0 skipped: TestKnown"},
		{"a parent that failed only through its known subtest", `{"Action":"run","Package":"p","Test":"TestP"}
{"Action":"run","Package":"p","Test":"TestP/bad"}
{"Action":"fail","Package":"p","Test":"TestP/bad"}
{"Action":"fail","Package":"p","Test":"TestP"}
{"Action":"fail","Package":"p"}
`, "1", Passed, "0 passed, 0 failed, 0 skipped, 2 known"},
		{"a parent with a new subtest beside its known one", `{"Action":"run","Package":"p","Test":"TestP"}
{"Action":"run","Package":"p","Test":"TestP/bad"}
{"Action":"fail","Package":"p","Test":"TestP/bad"}
{"Action":"run","Package":"p","Test":"TestP/worse"}
{"Action":"fail","Package":"p","Test":"TestP/worse"}
{"Action":"fail","Package":"p","Test":"TestP"}
{"Action":"fail","Package":"p"}
`, "1", Failed, "0 passed, 2 failed, 0 skipped, 1 known: TestP/worse"},
	} {
		t.Run(c.name, func(t *testing.T) {
			root, r := repo(t, "suites:\n  g: { cmd: go test ./..., format: go-json, known: k.yaml }\n",
				map[string]string{"k.yaml": "- { test: TestKnown, status: fail, fix: wontfix, category: env, reason: the clock steps }\n" +
					"- { test: TestP/bad, status: fail, fix: wontfix, category: env, reason: the disk fills }\n"})
			fakeGo(t, c.events, "exit "+c.code)
			if o := runIn(t, root, r, "g").Outcomes[0]; o.Result != c.result || o.Note != c.note {
				t.Errorf("%s %q, want %s %q", o.Result, o.Note, c.result, c.note)
			}
		})
	}
}
