package run

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/harness/registry"
)

// goModule is a module with tests that pass, fail, skip and fail in a
// subtest.
var goModule = map[string]string{
	"go.mod": "module example.com/m\n\ngo 1.21\n",
	"a/a_test.go": `package a

import "testing"

func TestPass(t *testing.T) {}
func TestFail(t *testing.T) { t.Fatal("broke") }
func TestSkip(t *testing.T) { t.Skip("nothing to check here") }
func TestParent(t *testing.T) {
	t.Run("good", func(t *testing.T) {})
	t.Run("bad", func(t *testing.T) { t.Fatal("sub broke") })
}
`,
}

// goRun runs target of a go-json registry y in a module of files, choosing
// tests, and returns the outcome and what the run printed.
func goRun(t *testing.T, y string, files map[string]string, tests ...string) (Outcome, string, string) {
	t.Helper()
	root, r := anyRepo(t, y, files)
	cells, err := Select(root, r, "g", "", "pr", nil, nil, tests)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	rep, err := Run(context.Background(), cells, Options{Root: root, Results: filepath.Join(root, "results"), Out: &out})
	if err != nil {
		t.Fatal(err)
	}
	return rep.Outcomes[0], out.String(), rep.Dir
}

// Each test and subtest is a test case: a failed one fails the cell, and the
// note counts them and names the failed ones, without a parent whose subtest
// failed. The tests' own output passes through as go test -v prints it, and
// the events stay in a file (sections 3 and 4.6).
func TestGoJSONGradesEachTest(t *testing.T) {
	o, out, dir := goRun(t, "suites:\n  g: { cmd: go test ./..., format: go-json }\n", goModule)
	if want := "2 passed, 3 failed, 1 skipped: TestFail, TestParent/bad"; o.Result != Failed || o.Note != want {
		t.Errorf("outcome %+v, want failed with %q", o, want)
	}
	if !strings.Contains(out, "--- FAIL: TestFail") || strings.Contains(out, `"Action"`) {
		t.Errorf("output isn't go test -v's:\n%s", out)
	}
	if events, err := os.ReadFile(filepath.Join(dir, "g", "go-test-1.json")); err != nil || !bytes.Contains(events, []byte(`"Action":"fail"`)) {
		t.Errorf("events file: %v", err)
	}
}

// -count=1 is added, so a second run doesn't replay the first from the cache.
func TestGoJSONNeverReplaysTheCache(t *testing.T) {
	files := map[string]string{"go.mod": goModule["go.mod"], "a/a_test.go": "package a\n\nimport \"testing\"\n\nfunc TestPass(t *testing.T) {}\n"}
	root, r := anyRepo(t, "suites:\n  g: { cmd: go test ./..., format: go-json }\n", files)
	cells, err := Select(root, r, "g", "", "pr", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		var out bytes.Buffer
		if _, err := Run(context.Background(), cells, Options{Root: root, Results: filepath.Join(root, "results"), Out: &out}); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "(cached)") {
			t.Fatalf("run %d replayed the cache:\n%s", i+1, out.String())
		}
	}
}

// A cell where no test ran didn't run, whether its -run matched nothing or
// the tests named on the command line don't exist; a run of only skips
// passes, since skips aren't graded (sections 4.4 and 9).
func TestGoJSONNothingRanAndSkips(t *testing.T) {
	y := "suites:\n  g: { cmd: go test ./..., format: go-json }\n"
	if o, _, _ := goRun(t, "suites:\n  g: { cmd: go test -run NoSuchTest ./..., format: go-json }\n", goModule); o.Result != NotRun || o.Note != "no tests ran" {
		t.Errorf("-run matching nothing: %+v", o)
	}
	if o, _, _ := goRun(t, y, goModule, "TestNoSuch"); o.Result != NotRun {
		t.Errorf("a test name that doesn't exist: %+v", o)
	}
	if o, _, _ := goRun(t, y, goModule, "TestPass", "TestSkip"); o.Result != Passed || o.Note != "1 passed, 0 failed, 1 skipped" {
		t.Errorf("named tests, one a skip: %+v", o)
	}
}

// A package that doesn't build fails the cell and says so.
func TestGoJSONBuildFailure(t *testing.T) {
	files := map[string]string{"go.mod": goModule["go.mod"], "b/b_test.go": "package b\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) { undefined() }\n"}
	o, _, _ := goRun(t, "suites:\n  g: { cmd: go test ./..., format: go-json }\n", files)
	if o.Result != Failed || !strings.Contains(o.Note, "example.com/m/b: build failed") {
		t.Errorf("outcome %+v", o)
	}
}

// A process left holding the events file can't hold dt: it reads to the
// file's end once the command exits.
func TestFollowedIgnoresLeftovers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the leftover is started through sh")
	}
	var res results
	var out bytes.Buffer
	start := time.Now()
	cmd := exec.Command("sh", "-c", `sleep 30 & echo '{"Action":"pass","Package":"p","Test":"TestA"}'`)
	if _, err := followed(cmd, filepath.Join(t.TempDir(), "events.json"), &res, &out); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("took %s: the leftover held dt", took)
	}
	if res.tests["p TestA"] != "pass" {
		t.Errorf("events read: %v", res.tests)
	}
}

// go test's flags split into the build's, the test binary's and the packages.
func TestSplitGoTest(t *testing.T) {
	build, test, pkgs := splitGoTest(strings.Fields("go test -tags=e2e -race -timeout=30m -run Kerberos -short -p 2 ./test/e2e/... ./x"))
	if !slices.Equal(build, []string{"-tags=e2e", "-race", "-p=2"}) ||
		!slices.Equal(test, []string{"-test.timeout=30m", "-test.run=Kerberos", "-test.short"}) ||
		!slices.Equal(pkgs, []string{"./test/e2e/...", "./x"}) {
		t.Errorf("build %q, test %q, packages %q", build, test, pkgs)
	}
}

// The root path builds the binaries as the user and runs each through
// test2json in its package's directory, with the suite's environment passed
// explicitly and the test flags as the binary's (section 3). It runs once
// without sudo, and once through sudo where passwordless sudo exists.
func TestGoTestAsRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the root path runs through env and sudo")
	}
	files := map[string]string{"go.mod": goModule["go.mod"], "a/a_test.go": `package a

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnv(t *testing.T) {
	if os.Getenv("DT_SUITE_VAR") != "passed" {
		t.Error("the suite's environment wasn't passed")
	}
	if wd, _ := os.Getwd(); filepath.Base(wd) != "a" {
		t.Errorf("ran in %s, not the package's directory", wd)
	}
	if !testing.Short() {
		t.Error("-short didn't reach the binary")
	}
	if os.Getenv("DT_WANT_ROOT") == "1" && os.Geteuid() != 0 {
		t.Error("the binary didn't run as root")
	}
}
`}
	root, _ := anyRepo(t, "suites:\n  g: { cmd: \"true\" }\n", files)
	check := func(t *testing.T, own ...string) {
		out := t.TempDir()
		var res results
		var buf bytes.Buffer
		tr := tester{cell: Cell{Unit: &registry.Suite{}}, ctx: context.Background(), dir: root, env: os.Environ(), own: own, vals: map[string]string{"out": out}, out: &buf}
		code, err := tr.goTestAsRoot(strings.Fields("go test -short ./..."), filepath.Join(out, "go-test-1"), &res)
		if err != nil || code != 0 || res.tests["example.com/m/a TestEnv"] != "pass" {
			t.Errorf("exit %d, %v, tests %v\n%s", code, err, res.tests, buf.String())
		}
	}
	t.Run("as the user", func(t *testing.T) {
		elevate = nil
		t.Cleanup(func() { elevate = []string{"sudo", "-n"} })
		check(t, "DT_SUITE_VAR=passed")
	})
	t.Run("through sudo", func(t *testing.T) {
		if os.Geteuid() == 0 || exec.Command("sudo", "-n", "true").Run() != nil {
			t.Skip("no passwordless sudo")
		}
		check(t, "DT_SUITE_VAR=passed", "DT_WANT_ROOT=1")
	})
}

// {packages} lists the packages whose tests the tag changes, and finding none
// is an error (section 4.1).
func TestGoPackages(t *testing.T) {
	root := t.TempDir()
	for name, body := range map[string]string{
		"go.mod":            goModule["go.mod"],
		"a/a_test.go":       "package a\n",
		"b/b_test.go":       "package b\n",
		"b/b_tag_test.go":   "//go:build integration\n\npackage b\n",
		"c/c.go":            "package c\n",
		"c/c_tag_test.go":   "//go:build integration\n\npackage c\n",
		"d/d_other_test.go": "//go:build other\n\npackage d\n",
	} {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := goPackages(root, "integration")
	if want := []string{"example.com/m/b", "example.com/m/c"}; err != nil || !slices.Equal(got, want) {
		t.Errorf("got %q, %v, want %q", got, err, want)
	}
	if _, err := goPackages(root, "nosuchtag"); err == nil {
		t.Error("a tag that changes nothing gave no error")
	}
}
