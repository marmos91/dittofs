package run

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/harness/registry"
	"github.com/marmos91/dittofs/test/harness/report"
)

// soon is a date the quarantine list takes as an expiry: a month ahead.
func soon() string {
	return time.Now().UTC().AddDate(0, 0, 30).Format(time.DateOnly)
}

// A test the quarantine list holds still runs, and its failure is recorded,
// graded quarantined, its JUnit skip saying so and naming the issue, but
// fails no cell and gets no annotation: in its
// suite's cells and its options', not in another suite's. A known-failure
// entry for the same test grades it first.
func TestQuarantinedTestNeverFails(t *testing.T) {
	root, r := repo(t, `suites:
  s: { cmd: "sh {test}", tests: "t/*.sh", options: { o: { cmd: "sh {test} again" } } }
  listed: { cmd: "sh {test}", tests: "t/*.sh", known: k.yaml }
  other: { cmd: "sh {test}", tests: "t/*.sh" }
`, map[string]string{
		"t/flaky.sh": "exit 1\n",
		"k.yaml":     "- { test: t/flaky.sh, status: fail, fix: wontfix, category: env, reason: r }\n",
		registry.QuarantinePath: "- { suite: s, test: t/flaky.sh, issue: '#1', expires: " + soon() + " }\n" +
			"- { suite: listed, test: t/flaky.sh, issue: '#1', expires: " + soon() + " }\n",
	})
	var cells []Cell
	for _, unit := range [][2]string{{"s", ""}, {"s", "o"}, {"listed", ""}, {"other", ""}} {
		c, err := Select(root, r, unit[0], unit[1], "pr", nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		cells = append(cells, c...)
	}
	var out bytes.Buffer
	rep, err := Run(context.Background(), cells, Options{Root: root, Results: filepath.Join(root, "results"), CI: true, Out: &out})
	if err != nil {
		t.Fatal(err)
	}
	var results []string
	for _, o := range rep.Outcomes {
		results = append(results, o.Name+": "+o.Result)
	}
	if want := []string{"s: passed", "s/o: passed", "listed: passed", "other: failed"}; !slices.Equal(results, want) {
		t.Errorf("cells %q, want %q", results, want)
	}
	want := []string{
		"s > t/flaky.sh: skipped [quarantined] | `sh t/flaky.sh` exited 1",
		"s o > t/flaky.sh: skipped [quarantined] | `sh t/flaky.sh again` exited 1",
		"listed > t/flaky.sh: skipped [known] | `sh t/flaky.sh` exited 1",
		"other > t/flaky.sh: failed [new] | `sh t/flaky.sh` exited 1",
	}
	if got := readSummary(t, filepath.Join(rep.Dir, "summary.json")).cases(); !slices.Equal(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	if junit, err := os.ReadFile(filepath.Join(rep.Dir, "junit", "s.xml")); err != nil || !strings.Contains(string(junit), `<skipped message="quarantined as flaky, see #1">`) {
		t.Errorf("s's JUnit file doesn't say the test is quarantined, and why: %v\n%s", err, junit)
	}
	var annotated []string
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "::error") {
			annotated = append(annotated, line)
		}
	}
	if len(annotated) != 1 || !strings.Contains(annotated[0], "title=dt other%3A t/flaky.sh") {
		t.Errorf("annotations %q, want one, for other's failure", annotated)
	}
}

// A go-json cell's quarantined failure passes it, and its note counts it as
// known, not as failed.
func TestGoJSONQuarantined(t *testing.T) {
	root, r := repo(t, "suites:\n  g: { cmd: go test ./..., format: go-json }\n",
		map[string]string{registry.QuarantinePath: "- { suite: g, test: TestFlaky, issue: '#1', expires: " + soon() + " }\n"})
	fakeGo(t, `{"Action":"run","Package":"p","Test":"TestFlaky"}
{"Action":"fail","Package":"p","Test":"TestFlaky"}
{"Action":"run","Package":"p","Test":"TestPass"}
{"Action":"pass","Package":"p","Test":"TestPass"}
{"Action":"fail","Package":"p"}
`, "exit 1")
	if o := runIn(t, root, r, "g").Outcomes[0]; o.Result != report.Passed || o.Note != "1 passed, 0 failed, 0 skipped, 1 known" {
		t.Errorf("%s %q, want passed, with the quarantined test counted as known", o.Result, o.Note)
	}
}

// From its date an expired entry excuses nothing: its test's failure is new
// again, and fails the cell.
func TestExpiredQuarantineExcusesNothing(t *testing.T) {
	root, r := repo(t, "suites:\n  s: { cmd: \"sh {test}\", tests: \"t/*.sh\" }\n", map[string]string{
		"t/flaky.sh":            "exit 1\n",
		registry.QuarantinePath: "- { suite: s, test: t/flaky.sh, issue: '#1', expires: " + time.Now().UTC().Format(time.DateOnly) + " }\n",
	})
	if o := runIn(t, root, r, "s").Outcomes[0]; o.Result != report.Failed || len(o.Tests) != 1 || o.Tests[0].Grade != report.New {
		t.Errorf("%s %q %+v, want failed, its test new", o.Result, o.Note, o.Tests)
	}
}
