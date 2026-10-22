package known

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/marmos91/dittofs/test/harness/report"
)

// Prune removes the entries the results show stale and leaves the rest of
// the file as it was. A removed entry's comment heads the entries after it, so the last
// such comment moves to the next entry kept, and goes with the last entry it
// headed. A file with nothing to remove isn't written.
func TestPrune(t *testing.T) {
	const file = "# The old failures.\n" +
		"- {test: a, status: fail, fix: wontfix, category: proto, reason: gone}\n" +
		"# The failures.\n" +
		"- {test: c, status: fail, fix: wontfix, category: proto, reason: gone}\n" +
		"- test: b\n  status: fail\n  fix: planned\n  category: bug\n  reason: 'still: failing'\n  issue: '#1'\n  where: {nfs: [\"3\"]}\n" +
		"# The skips.\n" +
		"- {test: d, status: skip, fix: wontfix, category: env, reason: kept}\n" +
		"# The last skip.\n" +
		"- {test: e, status: skip, fix: wontfix, category: env, reason: gone}\n"
	path := write(t, filepath.Join(t.TempDir(), "known.yaml"), file)

	passed := []Result{{Name: "a", Outcome: report.Passed}, {Name: "c", Outcome: report.Passed}, {Name: "e", Outcome: report.Passed}}
	removed, err := Prune(path, passed)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range removed {
		names = append(names, e.Test)
	}
	if !slices.Equal(names, []string{"a", "c", "e"}) {
		t.Errorf("removed %q, want a, c and e", names)
	}
	want := "# The failures.\n" +
		"- test: b\n  status: fail\n  fix: planned\n  category: bug\n  reason: 'still: failing'\n  issue: '#1'\n  where: {nfs: [\"3\"]}\n" +
		"# The skips.\n" +
		"- {test: d, status: skip, fix: wontfix, category: env, reason: kept}\n"
	if got, _ := os.ReadFile(path); string(got) != want {
		t.Errorf("file\n%s\nwant\n%s", got, want)
	}

	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	if removed, err := Prune(path, passed); err != nil || removed != nil {
		t.Errorf("nothing to remove: %v, %v", removed, err)
	}
}

// Prune judges an entry on the results that matched its test in cells it
// applies to: a known failure goes when one of them passed and none failed,
// an expected skip when one ran, passing or failing, and none skipped. A
// result with no outcome, or a skip of a known failure, shows nothing.
func TestPruneRule(t *testing.T) {
	const fail, skip = "{test: x, status: fail, fix: wontfix, category: proto, reason: r", "{test: x, status: skip, fix: wontfix, category: env, reason: r"
	result := func(outcomes ...string) []Result {
		var out []Result
		for _, o := range outcomes {
			out = append(out, Result{Name: "x", Outcome: o})
		}
		return out
	}
	for _, c := range []struct {
		name, entry string
		results     []Result
		removed     bool
	}{
		{"a known failure that passed goes", fail + "}", result(report.Passed), true},
		{"a known failure that also failed stays", fail + "}", result(report.Passed, report.Failed), false},
		{"a known failure that only skipped or had no result stays", fail + "}", result(report.Skipped, report.Other), false},
		{"an expected skip that ran and failed goes", skip + "}", result(report.Failed), true},
		{"an expected skip that also skipped stays", skip + "}", result(report.Passed, report.Skipped), false},
		{"an expected skip with no result stays", skip + "}", result(report.Other), false},
		{"an intermittent entry is never removed", fail + ", intermittent: true}", result(report.Passed), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := write(t, filepath.Join(t.TempDir(), "known.yaml"), "- "+c.entry+"\n")
			removed, err := Prune(path, c.results)
			if err != nil || (len(removed) == 1) != c.removed {
				t.Errorf("removed %v, %v; want removed %v", removed, err, c.removed)
			}
		})
	}
}
