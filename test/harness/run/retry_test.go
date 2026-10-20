package run

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/harness/registry"
	"github.com/marmos91/dittofs/test/harness/report"
)

// retryRun runs target at tier pr with retry retries, and returns its first
// cell.
func retryRun(t *testing.T, ctx context.Context, root string, r *registry.Registry, target string, retry int) report.Cell {
	t.Helper()
	cells, err := Select(root, r, target, "", "pr", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Run(ctx, cells, Options{Root: root, Results: filepath.Join(root, "results"), Out: io.Discard, Retry: retry})
	if err != nil {
		t.Fatal(err)
	}
	return rep.Outcomes[0]
}

// byName maps a cell's results by name.
func byName(c report.Cell) map[string]report.Test {
	m := map[string]report.Test{}
	for _, tc := range c.Tests {
		m[tc.Name] = tc
	}
	return m
}

// A retry runs only the tests that failed new again, each up to the number
// of retries: a known failure, a pass and an unexpected pass run once. One
// that passes on a retry is flaky, a pass named in the note, and one that
// fails every time stays a new failure; both count their retries.
func TestRetryRunsTheNewFailuresAgain(t *testing.T) {
	logRun := `echo x >> "$0.runs"` + "\n"
	root, r := repo(t, `suites:
  s: { cmd: "sh {test}", tests: "t/*.sh", known: k.yaml }
`, map[string]string{
		"t/broken.sh": logRun + "exit 1\n",
		"t/fixed.sh":  logRun + "exit 0\n",
		"t/flaky.sh":  logRun + `[ "$(wc -l < "$0.runs")" -gt 1 ]` + "\n",
		"t/known.sh":  logRun + "exit 1\n",
		"t/pass.sh":   logRun,
		"k.yaml": "- { test: t/known.sh, status: fail, fix: wontfix, category: env, reason: r }\n" +
			"- { test: t/fixed.sh, status: fail, fix: wontfix, category: env, reason: r }\n",
	})
	o := retryRun(t, context.Background(), root, r, "s", 2)
	for test, want := range map[string]int{"broken": 3, "fixed": 1, "flaky": 2, "known": 1, "pass": 1} {
		if runs, _ := os.ReadFile(filepath.Join(root, "t", test+".sh.runs")); len(runs) != 2*want {
			t.Errorf("t/%s.sh ran %d times, want %d", test, len(runs)/2, want)
		}
	}
	want := "t/broken.sh: `sh t/broken.sh` exited 1; 1 passed unexpectedly: t/fixed.sh; 1 flaky: t/flaky.sh"
	if o.Result != report.Failed || o.Note != want {
		t.Errorf("%s %q, want failed with %q", o.Result, o.Note, want)
	}
	tests := byName(o)
	if tc := tests["t/flaky.sh"]; tc.Status != report.Passed || tc.Grade != "" || !tc.Flaky || tc.Retries != 1 {
		t.Errorf("flaky: %+v", tc)
	}
	if tc := tests["t/broken.sh"]; tc.Grade != report.New || tc.Flaky || tc.Retries != 2 {
		t.Errorf("broken: %+v", tc)
	}
}

// A go-json retry runs the failed top-level tests again by -run, so a test
// that passed isn't run again. A parent whose new subtest passes on the retry
// is graded on what still fails under it: here a known subtest, so it is
// known too. The note counts the flaky tests apart.
func TestRetryGoJSON(t *testing.T) {
	root, r := anyRepo(t, "suites:\n  g: { cmd: go test ./..., format: go-json, known: k.yaml }\n", map[string]string{
		"go.mod": goModule["go.mod"],
		"k.yaml": "- { test: TestMixed/known, status: fail, fix: wontfix, category: env, reason: r }\n",
		"f/f_test.go": `package f

import (
	"os"
	"testing"
)

// flip fails the first time it runs.
func flip(t *testing.T, marker string) {
	if _, err := os.Stat(marker); err != nil {
		_ = os.WriteFile(marker, nil, 0o644)
		t.Fatal("fails the first time")
	}
}

func TestPass(t *testing.T) {
	f, _ := os.OpenFile("passes", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	_, _ = f.WriteString("x")
	_ = f.Close()
}
func TestFail(t *testing.T) { t.Fatal("always") }
func TestFlip(t *testing.T) { flip(t, "flip") }
func TestMixed(t *testing.T) {
	t.Run("flip", func(t *testing.T) { flip(t, "mixed") })
	t.Run("known", func(t *testing.T) { t.Fatal("always") })
}
`,
	})
	o := retryRun(t, context.Background(), root, r, "g", 1)
	if want := "1 passed, 1 failed, 0 skipped, 2 known, 2 flaky: TestFail"; o.Result != report.Failed || o.Note != want {
		t.Errorf("%s %q, want failed with %q", o.Result, o.Note, want)
	}
	if passes, _ := os.ReadFile(filepath.Join(root, "f", "passes")); string(passes) != "x" {
		t.Errorf("TestPass ran %d times, want once", len(passes))
	}
	tests := byName(o)
	if tc := tests["TestMixed/flip"]; tc.Status != report.Passed || !tc.Flaky || tc.Retries != 1 {
		t.Errorf("TestMixed/flip: %+v", tc)
	}
	if tc := tests["TestMixed"]; tc.Grade != report.Known || tc.Retries != 1 {
		t.Errorf("TestMixed: %+v", tc)
	}
}

// A tool that takes test names is retried by name, through {names}: what it
// gives for the tests it wasn't asked for doesn't count.
func TestRetryByName(t *testing.T) {
	root, r := repo(t, `suites:
  p: { cmd: "sh tool.sh {out} {names}", format: junit, names: all }
`, map[string]string{"tool.sh": `out=$1; shift
echo "$*" >> calls
fail='<failure message="broke"/>'
{
  echo '<testsuites><testsuite>'
  for code in CODE1 CODE2 CODE3; do
    case "$*" in
      all) [ $code = CODE2 ] && r= || r=$fail ;;
      *$code*) [ $code = CODE3 ] && r=$fail || r= ;;
      *) r='<skipped/>' ;;
    esac
    echo "<testcase code=\"$code\" name=\"$code\">$r</testcase>"
  done
  echo '</testsuite></testsuites>'
} > "$out/junit.xml"
`})
	o := retryRun(t, context.Background(), root, r, "p", 1)
	if calls, _ := os.ReadFile(filepath.Join(root, "calls")); string(calls) != "all\nCODE1 CODE3\n" {
		t.Errorf("the tool was called with:\n%s", calls)
	}
	if want := "1 failed: CODE3; 1 flaky: CODE1"; o.Result != report.Failed || o.Note != want {
		t.Errorf("%s %q, want failed with %q", o.Result, o.Note, want)
	}
	if tc := byName(o)["CODE2"]; tc.Status != report.Passed || tc.Retries != 0 {
		t.Errorf("CODE2, which passed: %+v", tc)
	}
}

// A result named apart from its test, as smbtorture's are, retries the test
// it came from. A test that timed out and then passes no longer fails the
// cell.
func TestRetryTheTestAResultCameFrom(t *testing.T) {
	root, r := repo(t, `suites:
  st: { cmd: "sh torture.sh {test}", format: subunit, tests: list.txt, timeout: { test: 1s } }
`, map[string]string{
		"list.txt": "smb2.oplock prefix=oplock\nsmb2.lock prefix=lock\n",
		"torture.sh": `echo "$1" >> calls
n=$(grep -c "^$1\$" calls)
case "$1" in
  smb2.oplock)
    echo "test: batch1"
    if [ "$n" = 1 ]; then printf 'failure: batch1 [\nbroke\n]\n'; else echo "success: batch1"; fi
    echo "test: batch2"
    echo "success: batch2" ;;
  smb2.lock)
    echo "test: rw"
    if [ "$n" = 1 ]; then sleep 30; fi
    echo "success: rw" ;;
esac
`,
	})
	o := retryRun(t, context.Background(), root, r, "st", 1)
	if calls, _ := os.ReadFile(filepath.Join(root, "calls")); string(calls) != "smb2.oplock\nsmb2.lock\nsmb2.oplock\nsmb2.lock\n" {
		t.Errorf("torture.sh was called with:\n%s", calls)
	}
	if want := "2 flaky: smb2.oplock.batch1, smb2.lock.rw"; o.Result != report.Passed || o.Note != want {
		t.Errorf("%s %q, want passed with %q", o.Result, o.Note, want)
	}
}

// An interrupt during a retry leaves the cell not run, as one during its
// first run does.
func TestRetryInterrupted(t *testing.T) {
	root, r := repo(t, `suites:
  s: { cmd: "sh {test}", tests: "t/*.sh" }
  g: { cmd: go test ./..., format: go-json }
`, map[string]string{
		"t/a.sh": `[ -e "$0.ran" ] && { touch retrying; sleep 30; }
touch "$0.ran"
exit 1
`,
		"go.mod": goModule["go.mod"],
		"f/f_test.go": `package f

import (
	"os"
	"testing"
	"time"
)

func TestHang(t *testing.T) {
	if _, err := os.Stat("ran"); err == nil {
		_ = os.WriteFile("../retrying", nil, 0o644)
		time.Sleep(30 * time.Second)
	}
	_ = os.WriteFile("ran", nil, 0o644)
	t.Fatal("fails the first time")
}
`,
	})
	for _, target := range []string{"s", "g"} {
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			for ctx.Err() == nil {
				if err := os.Remove(filepath.Join(root, "retrying")); err == nil {
					cancel()
				}
				time.Sleep(20 * time.Millisecond)
			}
		}()
		o := retryRun(t, ctx, root, r, target, 1)
		cancel()
		if o.Result != report.NotRun || !strings.Contains(o.Note, "interrupted") || len(o.Tests) != 0 {
			t.Errorf("%s: %+v", target, o)
		}
	}
}
