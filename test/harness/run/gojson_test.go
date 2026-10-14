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
	"strconv"
	"strings"
	"syscall"
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
// the events stay in a file.
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
// passes, since skips aren't graded.
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

// A TEST name no test ran under is named in the note, even when another name
// ran: the cell didn't run, or failed when a test failed. A subtest's name is
// checked whole, so a parent that ran doesn't hide its mistyped subtest.
func TestGoJSONNamedTestsThatDidntRun(t *testing.T) {
	y := "suites:\n  g: { cmd: go test ./..., format: go-json }\n"
	for _, c := range []struct {
		names        []string
		result, note string
	}{
		{[]string{"TestPass", "TestTypo"}, NotRun, "1 passed, 0 failed, 0 skipped; TestTypo didn't run"},
		{[]string{"TestParent/typo"}, NotRun, "1 passed, 0 failed, 0 skipped; TestParent/typo didn't run"},
		{[]string{"TestFail", "TestTypo"}, Failed, "0 passed, 1 failed, 0 skipped: TestFail; TestTypo didn't run"},
	} {
		if o, _, _ := goRun(t, y, goModule, c.names...); o.Result != c.result || o.Note != c.note {
			t.Errorf("%s: %+v, want %s with %q", strings.Join(c.names, " "), o, c.result, c.note)
		}
	}
}

// A test that failed under one of a cell's commands failed the cell, even
// when a later command passed it.
func TestGoJSONKeepsAFailure(t *testing.T) {
	files := map[string]string{"go.mod": goModule["go.mod"], "f/f_test.go": `package f

import (
	"os"
	"testing"
)

func TestFlip(t *testing.T) {
	if _, err := os.Stat("failed"); err != nil {
		_ = os.WriteFile("failed", nil, 0o644)
		t.Fatal("fails the first time")
	}
}
`}
	o, _, _ := goRun(t, "suites:\n  g: { cmd: [go test ./..., go test ./...], format: go-json }\n", files)
	if want := "0 passed, 1 failed, 0 skipped: TestFlip"; o.Result != Failed || o.Note != want {
		t.Errorf("outcome %+v, want failed with %q", o, want)
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
// file's end once the command exits, and the leftover is killed.
func TestFollowedIgnoresLeftovers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the leftover is started through sh")
	}
	var res results
	var out bytes.Buffer
	start := time.Now()
	pidFile := filepath.Join(t.TempDir(), "pid")
	cmd := exec.CommandContext(context.Background(), "sh", "-c", `sleep 30 & echo $! > `+pidFile+`; echo '{"Action":"pass","Package":"p","Test":"TestA"}'`)
	killTree(cmd)
	if _, err := followed(cmd, filepath.Join(t.TempDir(), "events.json"), &res, &out); err != nil {
		t.Fatal(err)
	}
	pid, _ := os.ReadFile(pidFile)
	if n, err := strconv.Atoi(strings.TrimSpace(string(pid))); err != nil {
		t.Errorf("leftover pid %q: %v", pid, err)
	} else {
		// A killed leftover lingers until init reaps it; one left alive
		// sleeps on for 30 seconds.
		p, _ := os.FindProcess(n)
		for end := time.Now().Add(5 * time.Second); p.Signal(syscall.Signal(0)) == nil && time.Now().Before(end); {
			time.Sleep(50 * time.Millisecond)
		}
		if p.Signal(syscall.Signal(0)) == nil {
			_ = p.Kill()
			t.Error("the leftover outlived the command")
		}
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("took %s: the leftover held dt", took)
	}
	if res.tests["p TestA"] != "pass" {
		t.Errorf("events read: %v", res.tests)
	}
}

// {packages} lists the packages whose tests the tag changes, and finding none
// is an error.
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

// fakeSudo puts a sudo on the PATH that drops -n, resets the environment as
// sudo does, marks it, and runs the rest as the user, so the root path runs
// without real sudo. As root, dt takes the plain path, so there's nothing to
// test.
func fakeSudo(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("the root path runs through sudo, as a user that isn't root")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\n[ \"$1\" = -n ] && shift\nexec env -i DT_FAKE_SUDO=1 \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "sudo"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// withBins adds the dfs and dfsctl a root suite needs built, as stubs.
func withBins(files map[string]string) map[string]string {
	out := map[string]string{"cmd/dfs/main.go": "package main\n\nfunc main() {}\n", "cmd/dfsctl/main.go": "package main\n\nfunc main() {}\n"}
	for k, v := range files {
		out[k] = v
	}
	return out
}

// rootModule has a package that checks how it was run, and two packages named
// nfs, one at a versioned path, which go names nfs.test both.
var rootModule = withBins(map[string]string{
	"go.mod": goModule["go.mod"],
	"a/a_test.go": `package a

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHow(t *testing.T) {
	for k, want := range map[string]string{
		"DT_FAKE_SUDO": "1", "DT_SPACES": "two words", "DT_QUOTE": "it's",
		"DITTOFS_E2E_SKIP_KERBEROS": "", "GOPROXY": "", "GOFLAGS": "", "GOCACHE": "", "GOMODCACHE": "", "TMPDIR": "",
	} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	for _, name := range []string{"dfs", "dfsctl"} {
		if _, err := os.Stat(filepath.Join(os.Getenv("DITTOFS_E2E_BIN"), name)); err != nil {
			t.Errorf("DITTOFS_E2E_BIN lacks %s: %v", name, err)
		}
	}
	if wd, _ := os.Getwd(); filepath.Base(wd) != "a" {
		t.Errorf("ran in %s, not the package's directory", wd)
	}
}

func TestOther(t *testing.T) {}
`,
	"nfs/v3/v3_test.go":     "package nfs\n\nimport \"testing\"\n\nfunc TestV3(t *testing.T) {}\n",
	"other/nfs/nfs_test.go": "package nfs\n\nimport \"testing\"\n\nfunc TestOtherNFS(t *testing.T) {}\n",
})

const rootSuite = `suites:
  g:
    cmd: go test ./...
    format: go-json
    needs: [root]
    env: { DT_SPACES: "two words", DT_QUOTE: "it's" }
`

// The root path runs every package's binary through go test's -exec under
// sudo, two packages named nfs included, in its package's directory, with the
// suite's environment, quotes and spaces intact, and DITTOFS_E2E_BIN, but none
// of the caller's own variables, Go's settings included, and leaves only its
// events behind; TEST names reach the binaries as -run.
func TestRootPath(t *testing.T) {
	fakeSudo(t)
	t.Setenv("DITTOFS_E2E_SKIP_KERBEROS", "1")
	o, out, dir := goRun(t, rootSuite, rootModule)
	if o.Result != Passed || o.Note != "4 passed, 0 failed, 0 skipped" {
		t.Errorf("outcome %+v\n%s", o, out)
	}
	if left, _ := os.ReadDir(filepath.Join(dir, "g")); len(left) != 1 || left[0].Name() != "go-test-1.json" {
		t.Errorf("left in the cell's results: %v", left)
	}
	if o, _, _ := goRun(t, rootSuite, rootModule, "TestHow", "TestV3"); o.Result != Passed || o.Note != "2 passed, 0 failed, 0 skipped" {
		t.Errorf("with TEST names: %+v", o)
	}
}

// Cancelling the root path sends SIGINT, so the tests' own cleanup runs, and
// the cell didn't run: an interrupt is no failure.
func TestRootPathCancel(t *testing.T) {
	fakeSudo(t)
	dir := t.TempDir()
	started, cleaned := filepath.Join(dir, "started"), filepath.Join(dir, "cleaned")
	files := withBins(map[string]string{"go.mod": goModule["go.mod"], "w/w_test.go": `package w

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
`})
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
	if o := rep.Outcomes[0]; o.Result != NotRun || !strings.Contains(o.Note, "interrupted") || rep.Status() != 2 {
		t.Errorf("outcome %+v, status %d", o, rep.Status())
	}
}

// A go-json cell stopped at its limit failed, as a test that timed out does.
func TestGoJSONCellLimit(t *testing.T) {
	minute = 50 * time.Millisecond
	t.Cleanup(func() { minute = time.Minute })
	files := map[string]string{"go.mod": goModule["go.mod"], "w/w_test.go": "package w\n\nimport (\n\t\"testing\"\n\t\"time\"\n)\n\nfunc TestWait(t *testing.T) { time.Sleep(time.Minute) }\n"}
	o, _, _ := goRun(t, "suites:\n  g: { cmd: go test ./..., format: go-json }\n", files)
	if o.Result != Failed || !strings.Contains(o.Note, "stopped at the cell's limit") {
		t.Errorf("outcome %+v", o)
	}
}

// A package that fails outside any test fails the cell, named as such: one
// whose TestMain exits non-zero after its tests pass, and one whose goroutine
// panics before any test. A test that calls os.Exit(0) fails, since go test
// passes -test.paniconexit0. The same on both paths.
func TestOutsideAnyTest(t *testing.T) {
	files := withBins(map[string]string{
		"go.mod":        goModule["go.mod"],
		"tm/tm_test.go": "package tm\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\nfunc TestMain(m *testing.M) { m.Run(); os.Exit(3) }\n\nfunc TestFine(t *testing.T) {}\n",
		"gp/gp_test.go": "package gp\n\nimport (\n\t\"os\"\n\t\"testing\"\n\t\"time\"\n)\n\nfunc TestMain(m *testing.M) {\n\tgo func() { panic(\"boom\") }()\n\ttime.Sleep(time.Second)\n\tos.Exit(m.Run())\n}\n\nfunc TestNever(t *testing.T) {}\n",
		"ex/ex_test.go": "package ex\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\nfunc TestExit0(t *testing.T) { os.Exit(0) }\n",
	})
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

// TEST names select subtests too: each element of a name is anchored, and
// names are joined with |. A subtest is named alone, since one pattern for
// several names would filter each matched test's subtests.
func TestRunPatternSubtests(t *testing.T) {
	for names, want := range map[string]string{"TestParent/bad": "^TestParent$/^bad$", "TestA TestB": "^TestA$|^TestB$"} {
		if got := runPattern(strings.Fields(names)); got != want {
			t.Errorf("pattern for %s: %q, want %q", names, got, want)
		}
	}
	root, r := anyRepo(t, "suites:\n  g: { cmd: go test ./..., format: go-json }\n", goModule)
	if _, err := Select(root, r, "g", "", "pr", nil, nil, []string{"TestPass", "TestParent/bad"}); err == nil || err.Error() != "name subtests one at a time" {
		t.Errorf("a subtest among other names: %v", err)
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

// dfs and dfsctl are built once per run, for every root cell of it.
func TestBinOncePerRun(t *testing.T) {
	fakeSudo(t)
	files := withBins(map[string]string{"go.mod": goModule["go.mod"], "a/a_test.go": "package a\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\n"})
	y := "groups: { both: [g, h] }\nsuites:\n  g: { cmd: go test ./a/..., format: go-json, needs: [root] }\n  h: { cmd: go test ./a/..., format: go-json, needs: [root] }\n"
	root, r := anyRepo(t, y, files)
	cells, err := Select(root, r, "both", "", "pr", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	rep, err := Run(context.Background(), cells, Options{Root: root, Results: filepath.Join(root, "results"), Out: &out})
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out.String(), "[dt] $ go build"); n != 1 || rep.Status() != 0 {
		t.Errorf("%d builds, status %d\n%s", n, rep.Status(), out.String())
	}
}

// When dfs or dfsctl doesn't build, the root cell didn't run, and its log
// has the build's output.
func TestBinBuildFailure(t *testing.T) {
	fakeSudo(t)
	files := withBins(map[string]string{"go.mod": goModule["go.mod"], "a/a_test.go": "package a\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\n"})
	files["cmd/dfs/main.go"] = "package main\n\nfunc main() { notDefined() }\n"
	o, _, dir := goRun(t, "suites:\n  g: { cmd: go test ./..., format: go-json, needs: [root] }\n", files)
	log, _ := os.ReadFile(filepath.Join(dir, "logs", "g.log"))
	if o.Result != NotRun || !strings.Contains(o.Note, "dfs and dfsctl didn't build") || !strings.Contains(string(log), "notDefined") {
		t.Errorf("outcome %+v\nlog:\n%s", o, log)
	}
}

// {bin} is there for a root cell's commands; elsewhere dt doesn't provide it,
// and a command it can't fill leaves the cell not run.
func TestBinPlaceholder(t *testing.T) {
	fakeSudo(t)
	files := withBins(map[string]string{"go.mod": goModule["go.mod"], "a/a_test.go": "package a\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\n"})
	if o, out, _ := goRun(t, "suites:\n  g: { cmd: \"go test ./a/... -args {bin}\", format: go-json, needs: [root] }\n", files); o.Result != Passed {
		t.Errorf("root cell: %+v\n%s", o, out)
	}
	if o, _, _ := goRun(t, "suites:\n  g: { cmd: \"go test ./a/... -args {bin}\", format: go-json }\n", files); o.Result != NotRun || !strings.Contains(o.Note, "{bin}") {
		t.Errorf("a cell that doesn't run as root: %+v", o)
	}
}
