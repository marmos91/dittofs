package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

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

// dt plan --json gives every cell a name, a place and a limit.
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
	for _, c := range got.Cells {
		if c.Name == "" || c.Place == "" || c.TimeoutMinutes == 0 {
			t.Errorf("incomplete cell %+v", c)
		}
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

// A usage error is an error, which main turns into exit status 3.
func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{{"plan"}, {"plan", "--tier", "weekly"}, {"list", "--tier", "weekly"}} {
		if _, err := dt(t, args...); err == nil {
			t.Errorf("dt %s: no error", strings.Join(args, " "))
		}
	}
}

func TestListJSON(t *testing.T) {
	out, err := dt(t, "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Suites []struct{ Name, Tier string } `json:"suites"`
		Groups map[string][]string           `json:"groups"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Suites) == 0 || len(got.Groups["conformance"]) == 0 {
		t.Fatalf("list: %+v", got)
	}
}

func lines(s string) int { return strings.Count(s, "\n") }

// A renamed file is listed under both paths, so moving it out of a suite's
// paths still runs that suite.
func TestChangedFilesKeepsBothSidesOfARename(t *testing.T) {
	// A git hook running the tests exports GIT_DIR and the like, which would
	// point these commands at the repository being committed to.
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "GIT_") {
			t.Setenv(k, "")
			_ = os.Unsetenv(k)
		}
	}
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		pre := []string{"-C", root, "-c", "user.name=dt", "-c", "user.email=dt@example.com", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}
		if out, err := exec.Command("git", append(pre, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "pkg/old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pkg/old/x.go"), []byte("package old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
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
}

// git -z output keeps a path with spaces or quotes whole.
func TestSplitNUL(t *testing.T) {
	got := splitNUL([]byte("docs/a b.md\x00pkg/\"q\".go\x00"))
	if want := []string{"docs/a b.md", `pkg/"q".go`}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := splitNUL(nil); got == nil || len(got) != 0 {
		t.Errorf("no changes: %#v, want an empty, non-nil list", got)
	}
}

// dt run's usage errors are plain errors, which main turns into exit status
// 3; a run that didn't pass carries its own status.
func TestRunStatuses(t *testing.T) {
	for _, args := range [][]string{
		{"run"}, {"run", "nope"}, {"run", "lint", "--kerberos"},
		{"run", "smbtorture", "--kerberos", "--windows"}, {"run", "unit", "--tier", "weekly"},
	} {
		var status exitStatus
		if _, err := dt(t, args...); err == nil || errors.As(err, &status) {
			t.Errorf("dt %s: %v, want a usage error", strings.Join(args, " "), err)
		}
	}
	// pjdfstest needs a fixture, which dt doesn't set up yet, so it didn't run.
	var status exitStatus
	if _, err := dt(t, "run", "pjdfstest", "--results", t.TempDir()); !errors.As(err, &status) || status != 2 {
		t.Errorf("dt run pjdfstest: %v, want exit status 2", err)
	}
}

// Each suite option is a flag of dt run.
func TestRunOptionFlags(t *testing.T) {
	out, err := dt(t, "run", "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"--kerberos", "--windows", "--knfsd"} {
		if !strings.Contains(out, flag) {
			t.Errorf("dt run --help lacks %s", flag)
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
