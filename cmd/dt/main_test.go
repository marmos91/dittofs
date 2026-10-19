package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/test/harness/needs"
	"github.com/marmos91/dittofs/test/harness/plan"
)

func dt(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	root := newRoot(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

// dt plan --json gives the tier and its cells.
func TestPlanJSON(t *testing.T) {
	out, err := dt(t, "plan", "--tier", "pr", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Tier  string      `json:"tier"`
		Cells []plan.Cell `json:"cells"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got.Tier != "pr" || len(got.Cells) == 0 {
		t.Fatalf("plan: %+v", got)
	}
}

// --shard I/N keeps every N-th cell from the I-th, and refuses a bad spec.
func TestPlanShard(t *testing.T) {
	all, _ := dt(t, "plan", "--tier", "pr")
	one, err := dt(t, "plan", "--tier", "pr", "--shard", "1/2")
	if err != nil {
		t.Fatal(err)
	}
	if lines(one) >= lines(all) {
		t.Errorf("shard 1/2 has %d lines, the whole plan %d", lines(one), lines(all))
	}
	if _, err := dt(t, "plan", "--tier", "pr", "--shard", "3/2"); err == nil {
		t.Error("--shard 3/2 accepted")
	}
}

func lines(s string) int { return strings.Count(s, "\n") }

// A renamed file is listed under both paths, so moving it out of a suite's
// paths still runs that suite.
func TestChangedFilesKeepsBothSidesOfARename(t *testing.T) {
	gitless(t)
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		pre := []string{"-C", root, "-c", "user.name=dt", "-c", "user.email=dt@example.com", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}
		if out, err := exec.Command("git", append(pre, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	writeFile(t, filepath.Join(root, "pkg/old/x.go"), "package old\n")
	git("init", "-q")
	git("add", ".")
	git("commit", "-q", "-m", "add")
	git("mv", "pkg/old", "pkg/new")
	git("commit", "-q", "-m", "move")

	got, err := changedFiles(root, "HEAD~1")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"pkg/new/x.go", "pkg/old/x.go"}; !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, err := changedFiles(root, "HEAD"); err != nil || got == nil || len(got) != 0 {
		t.Errorf("no changes: %#v, %v, want an empty, non-nil list", got, err)
	}
}

// checkout moves the test into a new checkout whose registry has a suite
// with an option, one that fails, one that needs another platform, one with a
// fixture dt can't set up yet, one that needs this platform and whose 9x
// tests, which the merge tier doesn't select, need the dedicated host, and a
// group, so a dt run there runs only these. Checking their needs probes
// nothing on the machine. It returns this platform's need and the other one.
func checkout(t *testing.T) (here, other string) {
	t.Helper()
	here, other = "linux", "windows"
	switch runtime.GOOS {
	case "windows":
		here, other = "windows", "linux"
	case "darwin":
		here = "macos"
	}
	y := "version: 1\ndefaults: { format: exit, tier: pr, timeout: { test: 30m, cell: 60m } }\ngroups: { g: [s, away] }\nsuites:\n" +
		"  s: { cmd: \"true\", options: { o: { cmd: \"true\" } } }\n" +
		"  fails: { cmd: \"false\" }\n" +
		"  away: { cmd: \"true\", needs: [" + other + "] }\n" +
		"  fixed: { cmd: \"true\", fixture: { profile: memory, smb: { mount: true } } }\n" +
		"  pictured: { image: x, needs: [" + here + "] }\n" +
		"  partly: { cmd: \"run {test}\", tests: \"t/*.sh\", select: { merge: \"0?-*.sh\" }, needs: [" + here + "], needs_by_test: { \"9?-*.sh\": [\"host:dedicated\", " + here + "] } }\n"
	root := t.TempDir()
	for _, test := range []string{"01-a.sh", "91-b.sh"} {
		writeFile(t, filepath.Join(root, "t", test), "")
	}
	writeFile(t, filepath.Join(root, "test", "suites.yaml"), y)
	t.Chdir(root)
	return here, other
}

// Usage errors are plain errors, which main turns into exit status 3; a run
// that didn't pass carries its own status.
func TestRunStatuses(t *testing.T) {
	for _, args := range [][]string{
		{"plan", "--tier", "weekly"}, {"list", "--tier", "weekly"}, {"run"},
		{"run", "smbtorture", "--kerberos", "--windows"}, {"run", "unit", "--tier", "weekly"},
	} {
		var status exitStatus
		if _, err := dt(t, args...); err == nil || errors.As(err, &status) {
			t.Errorf("dt %s: %v, want a usage error", strings.Join(args, " "), err)
		}
	}
	checkout(t)
	want := map[string]exitStatus{"away": 2} // skipped: nothing ran
	if runtime.GOOS != "windows" {
		want["fails"] = 1
	}
	for suite, code := range want {
		var status exitStatus
		if _, err := dt(t, "run", suite, "--results", t.TempDir()); !errors.As(err, &status) || status != code {
			t.Errorf("dt run %s: %v, want exit status %d", suite, err, code)
		}
	}
}

// An option's flag runs its cells only when it's true.
func TestRunOptionFlagChoosesItsCells(t *testing.T) {
	checkout(t)
	for args, want := range map[string]string{"--o": "[dt] s/o:", "--o=false": "[dt] s:"} {
		out, _ := dt(t, "run", "s", args, "--results", t.TempDir())
		if !strings.Contains(out, want) || (args == "--o=false" && strings.Contains(out, "s/o")) {
			t.Errorf("dt run s %s:\n%s", args, out)
		}
	}
}

// The shim passes dt's exit status through, from wherever it's called.
func TestShimKeepsExitStatus(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the shim is a sh script")
	}
	shim, err := filepath.Abs("../../test/harness/bin/dt")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(shim, "run", "nope")
	cmd.Dir = t.TempDir()
	err = cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Errorf("shim: %v, want exit status 3", err)
	}
}

// dt unit, dt integration and dt e2e are dt run of that suite with the rest
// of the arguments, as before: each is given an option only another suite
// has, so a wrong mapping names the wrong suite.
func TestAliases(t *testing.T) {
	for name, option := range map[string]string{"unit": "knfsd", "integration": "windows", "e2e": "windows"} {
		want := fmt.Sprintf("%s has no option %q", name, option)
		if _, err := dt(t, name, "--"+option); err == nil || err.Error() != want {
			t.Errorf("dt %s --%s: %v, want %q", name, option, err, want)
		}
	}
}

// dt list says whether dt run would run each suite here, as dt run decides
// it: first what dt can't do yet, then the suite's needs, and per
// needs_by_test glob whose tests the tier plans, what those tests need beyond
// them. A suite with an image runs where its needs are met. It lists the
// groups too.
func TestListRunsHere(t *testing.T) {
	_, other := checkout(t)
	out, err := dt(t, "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	type suite struct {
		RunsHere    bool                `json:"runs_here"`
		Unsupported string              `json:"unsupported"`
		Unmet       []string            `json:"unmet"`
		UnmetByTest map[string][]string `json:"unmet_by_test"`
	}
	var got struct {
		Suites []struct {
			Name string `json:"name"`
			suite
		} `json:"suites"`
		Groups map[string][]string `json:"groups"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	fixtures := "dt doesn't mount SMB yet"
	want := map[string]suite{
		"s":        {RunsHere: true},
		"s/o":      {RunsHere: true},
		"fails":    {RunsHere: true},
		"away":     {Unmet: []string{other}},
		"fixed":    {Unsupported: fixtures},
		"pictured": {RunsHere: true},
		"partly":   {RunsHere: true, UnmetByTest: map[string][]string{"9?-*.sh": {"host:dedicated"}}},
	}
	for _, s := range got.Suites {
		if w, ok := want[s.Name]; !ok || !reflect.DeepEqual(s.suite, w) {
			t.Errorf("%s: %+v, want %+v", s.Name, s.suite, w)
		}
		delete(want, s.Name)
	}
	if len(want) > 0 || !slices.Equal(got.Groups["g"], []string{"s", "away"}) {
		t.Errorf("missing %v, groups %v:\n%s", want, got.Groups, out)
	}
	text, _ := dt(t, "list")
	for _, line := range []string{"HERE", "no: " + other, "no: " + fixtures, "yes; not 9?-*.sh: host:dedicated"} {
		if !strings.Contains(text, line) {
			t.Errorf("dt list lacks %q:\n%s", line, text)
		}
	}
	if merge, _ := dt(t, "list", "--tier", "merge"); strings.Contains(merge, "9?-*.sh") {
		t.Errorf("the merge tier selects no 9x test:\n%s", merge)
	}
	if run, _ := dt(t, "run", "fixed", "--results", t.TempDir()); !strings.Contains(run, "[dt] fixed: not run "+fixtures) {
		t.Errorf("dt run fixed doesn't give dt list's reason:\n%s", run)
	}
}

// dt doctor lists every need the registry names, per-test needs included,
// whether this machine meets it, and how to meet one it doesn't.
func TestDoctor(t *testing.T) {
	here, other := checkout(t)
	out, err := dt(t, "doctor")
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string][]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n")[1:] {
		f := strings.Fields(line)
		rows[f[0]] = f[1:]
	}
	if len(rows) != 3 || len(rows[here]) != 1 || rows[here][0] != "yes" {
		t.Errorf("want rows for %s, %s and host:dedicated, %s met:\n%s", here, other, here, out)
	}
	for _, unmet := range []string{other, "host:dedicated"} {
		if row := strings.Join(rows[unmet], " "); !strings.HasPrefix(row, "no ") || !strings.HasSuffix(row, needs.Hint(unmet)) {
			t.Errorf("%s: %q, want unmet, why, and %q", unmet, row, needs.Hint(unmet))
		}
	}
}

// --results is relative to the checkout's root, wherever dt runs from, and
// the run's summary.json records the tier the run was asked for.
func TestRunResults(t *testing.T) {
	checkout(t)
	if err := os.Mkdir("sub", 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir("sub")
	_, _ = dt(t, "run", "away", "--tier", "merge", "--results", "rel")
	files, _ := filepath.Glob(filepath.Join("..", "rel", "*", "summary.json"))
	if len(files) != 1 {
		t.Fatalf("summaries under the root's rel: %v", files)
	}
	var doc struct {
		Results struct{ Extra struct{ Tier string } }
	}
	if data, err := os.ReadFile(files[0]); err != nil || json.Unmarshal(data, &doc) != nil || doc.Results.Extra.Tier != "merge" {
		t.Errorf("tier %q, %v", doc.Results.Extra.Tier, err)
	}
}

// When summary.json can't be written, dt run still prints how each cell went,
// then fails with the error.
func TestRunReportsWhenTheSummaryFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the suite runs sh")
	}
	y := "version: 1\ndefaults: { format: exit, tier: pr, timeout: { test: 30m, cell: 60m } }\nsuites:\n" +
		"  breaks: { cmd: { sh: \"rm {out}/../summary.json && mkdir -p {out}/../summary.json/x\" } }\n"
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "test", "suites.yaml"), y)
	t.Chdir(root)
	out, err := dt(t, "run", "breaks", "--ci", "--results", t.TempDir())
	var status exitStatus
	if err == nil || errors.As(err, &status) || !strings.Contains(out, "breaks  passed") {
		t.Errorf("dt run: %v\n%s", err, out)
	}
}
