package run

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
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

// fakeSudo puts a sudo on the PATH that drops -n, marks the environment and
// runs the rest as the user, so the root path runs without real sudo. As
// root, dt takes the plain path, so there's nothing to test.
func fakeSudo(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("the root path runs through sudo, as a user that isn't root")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\n[ \"$1\" = -n ] && shift\nDT_FAKE_SUDO=1 exec \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "sudo"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// rootModule has a package that checks how it was run, and two packages named
// nfs, one at a versioned path, which go names nfs.test both.
var rootModule = map[string]string{
	"go.mod": goModule["go.mod"],
	"a/a_test.go": `package a

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHow(t *testing.T) {
	for k, want := range map[string]string{
		"DT_FAKE_SUDO": "1", "DT_SPACES": "two words", "DT_QUOTE": "it's",
		"GOPROXY": "off", "GOFLAGS": "-mod=readonly",
	} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if !strings.Contains(os.Getenv("GOCACHE"), "dt-root-gocache") {
		t.Errorf("GOCACHE = %q, not root's own", os.Getenv("GOCACHE"))
	}
	if wd, _ := os.Getwd(); filepath.Base(wd) != "a" {
		t.Errorf("ran in %s, not the package's directory", wd)
	}
}

func TestOther(t *testing.T) {}
`,
	"nfs/v3/v3_test.go":     "package nfs\n\nimport \"testing\"\n\nfunc TestV3(t *testing.T) {}\n",
	"other/nfs/nfs_test.go": "package nfs\n\nimport \"testing\"\n\nfunc TestOtherNFS(t *testing.T) {}\n",
}

const rootSuite = `suites:
  g:
    cmd: go test ./...
    format: go-json
    needs: [root]
    env: { DT_SPACES: "two words", DT_QUOTE: "it's" }
`

// The root path runs every package's binary through go test's -exec under
// sudo, two packages named nfs included, in its package's directory, with the
// suite's environment, quotes and spaces intact, and root's build settings;
// TEST names reach the binaries as -run (section 3).
func TestRootPath(t *testing.T) {
	fakeSudo(t)
	o, out, _ := goRun(t, rootSuite, rootModule)
	if o.Result != Passed || o.Note != "4 passed, 0 failed, 0 skipped" {
		t.Errorf("outcome %+v\n%s", o, out)
	}
	if o, _, _ := goRun(t, rootSuite, rootModule, "TestHow", "TestV3"); o.Result != Passed || o.Note != "2 passed, 0 failed, 0 skipped" {
		t.Errorf("with TEST names: %+v", o)
	}
}

// Cancelling the root path sends SIGINT, so the tests' own cleanup runs.
func TestRootPathCancel(t *testing.T) {
	fakeSudo(t)
	dir := t.TempDir()
	started, cleaned := filepath.Join(dir, "started"), filepath.Join(dir, "cleaned")
	files := map[string]string{"go.mod": goModule["go.mod"], "w/w_test.go": `package w

import (
	"os"
	"os/signal"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		<-c
		_ = os.WriteFile(os.Getenv("DT_CLEANED"), nil, 0o644)
		os.Exit(1)
	}()
	os.Exit(m.Run())
}

func TestWait(t *testing.T) {
	_ = os.WriteFile(os.Getenv("DT_STARTED"), nil, 0o644)
	time.Sleep(time.Minute)
}
`}
	y := "suites:\n  g: { cmd: go test ./..., format: go-json, needs: [root], env: { DT_STARTED: \"" + started + "\", DT_CLEANED: \"" + cleaned + "\" } }\n"
	root, r := anyRepo(t, y, files)
	cells, err := Select(root, r, "g", "", "pr", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			if _, err := os.Stat(started); err == nil {
				cancel()
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	rep, err := Run(ctx, cells, Options{Root: root, Results: filepath.Join(root, "results"), Out: &bytes.Buffer{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cleaned); err != nil {
		t.Error("the tests' cleanup didn't run: they weren't interrupted")
	}
	if o := rep.Outcomes[0]; o.Result != Failed || !strings.Contains(o.Note, "interrupted") {
		t.Errorf("outcome %+v", o)
	}
}

// A package that fails outside any test fails the cell, named as such: one
// whose TestMain exits non-zero after its tests pass, and one whose goroutine
// panics before any test. A test that calls os.Exit(0) fails, since go test
// passes -test.paniconexit0. The same on both paths.
func TestOutsideAnyTest(t *testing.T) {
	files := map[string]string{
		"go.mod":        goModule["go.mod"],
		"tm/tm_test.go": "package tm\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\nfunc TestMain(m *testing.M) { m.Run(); os.Exit(3) }\n\nfunc TestFine(t *testing.T) {}\n",
		"gp/gp_test.go": "package gp\n\nimport (\n\t\"os\"\n\t\"testing\"\n\t\"time\"\n)\n\nfunc TestMain(m *testing.M) {\n\tgo func() { panic(\"boom\") }()\n\ttime.Sleep(time.Second)\n\tos.Exit(m.Run())\n}\n\nfunc TestNever(t *testing.T) {}\n",
		"ex/ex_test.go": "package ex\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\nfunc TestExit0(t *testing.T) { os.Exit(0) }\n",
	}
	check := func(t *testing.T, needs string) {
		o, out, _ := goRun(t, "suites:\n  g: { cmd: go test ./..., format: go-json, needs: ["+needs+"] }\n", files)
		for _, want := range []string{"TestExit0", "example.com/m/gp: failed outside any test", "example.com/m/tm: failed outside any test"} {
			if o.Result != Failed || !strings.Contains(o.Note, want) {
				t.Errorf("note %q lacks %q\n%s", o.Note, want, out)
			}
		}
	}
	t.Run("as the user", func(t *testing.T) { check(t, "") })
	t.Run("as root", func(t *testing.T) {
		fakeSudo(t)
		check(t, "root")
	})
}

// TEST names select subtests too: each element of a name is anchored.
func TestRunPatternSubtests(t *testing.T) {
	if got, want := runPattern([]string{"TestA", "TestParent/bad"}), "^TestA$|^TestParent$/^bad$"; got != want {
		t.Errorf("pattern %q, want %q", got, want)
	}
	o, _, _ := goRun(t, "suites:\n  g: { cmd: go test ./..., format: go-json }\n", goModule, "TestParent/bad")
	if o.Result != Failed || o.Note != "0 passed, 2 failed, 0 skipped: TestParent/bad" {
		t.Errorf("outcome %+v", o)
	}
}

// -exec takes a word in quotes whole, so a value with spaces or a quote is
// quoted with the quote it doesn't hold; one with both can't be passed.
func TestExecQuote(t *testing.T) {
	for w, want := range map[string]string{"plain": "plain", "two words": "'two words'", "it's": `"it's"`, "": "''"} {
		if got, err := execQuote(w); err != nil || got != want {
			t.Errorf("execQuote(%q) = %q, %v, want %q", w, got, err, want)
		}
	}
	if _, err := execQuote(`it's "both"`); err == nil {
		t.Error("a word with both quotes passed")
	}
}

// A service whose variables aren't set falls back to the local service's,
// which reach the tests' environment: here KMIP's, from the file dt services
// writes, pointing at a listener of the test's own.
func TestGoJSONServiceEnv(t *testing.T) {
	t.Setenv("DITTOFS_TEST_KMIP", "")
	t.Setenv("DITTOFS_TEST_KMIP_ENDPOINT", "")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	files := map[string]string{
		"go.mod":                      goModule["go.mod"],
		"test/harness/state/kmip.env": "DITTOFS_TEST_KMIP=1\nDITTOFS_TEST_KMIP_ENDPOINT=" + l.Addr().String() + "\n",
		"k/k_test.go": `package k

import (
	"os"
	"testing"
)

func TestEndpoint(t *testing.T) {
	if os.Getenv("DITTOFS_TEST_KMIP") != "1" || os.Getenv("DITTOFS_TEST_KMIP_ENDPOINT") == "" {
		t.Error("the local KMIP's variables didn't reach the tests")
	}
}
`,
	}
	o, out, _ := goRun(t, "suites:\n  g: { cmd: go test ./..., format: go-json, needs: [\"service:kmip\"] }\n", files)
	if o.Result != Passed {
		t.Errorf("outcome %+v\n%s", o, out)
	}
}
