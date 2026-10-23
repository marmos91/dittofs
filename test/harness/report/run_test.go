package report_test

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/test/harness/registry"
	"github.com/marmos91/dittofs/test/harness/report"
	"github.com/marmos91/dittofs/test/harness/run"
)

// A real run's summary.json is CTRF 0.1.0 by CTRF's own schema, with a
// result for every test: exit tests and commands that passed, failed or
// skipped, and Go tests and subtests that passed, failed or skipped. Each
// cell's JUnit file, which summary.json names, has a testcase for each of the
// cell's results. It names the kernel the run was on, as uname -r does.
func TestRealRunMatchesTheSchema(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the exit suites run sh")
	}
	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("GITHUB_EVENT_NAME", "schedule") // so a command using {base} skips
	root := t.TempDir()
	for name, body := range map[string]string{
		"t/pass.sh": "exit 0\n",
		"t/fail.sh": "exit 1\n",
		"go.mod":    "module example.com/m\n\ngo 1.21\n",
		"a/a_test.go": `package a

import "testing"

func TestPass(t *testing.T)   {}
func TestFail(t *testing.T)   { t.Fatal("broke") }
func TestSkip(t *testing.T)   { t.Skip("nothing to check here") }
func TestParent(t *testing.T) { t.Run("sub", func(t *testing.T) {}) }
`,
	} {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r, err := registry.Parse(root, []byte(`version: 1
defaults: { format: exit, tier: pr, timeout: { test: 30m, cell: 60m } }
groups: { g: [files, commands, gotests] }
suites:
  files: { cmd: "sh {test}", tests: "t/*.sh" }
  commands: { cmd: [ "true", "false", "no-such-check {base}" ] }
  gotests: { cmd: go test ./..., format: go-json }
`))
	if err != nil {
		t.Fatal(err)
	}
	cells, err := run.Select(root, r, "g", "", "pr", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := run.Run(context.Background(), cells, run.Options{Root: root, Results: filepath.Join(root, "results"), Out: io.Discard, Tier: "pr"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(rep.Dir, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	if errs := report.CheckCTRF(t, data); len(errs) > 0 {
		t.Errorf("summary.json breaks the schema:\n%s", strings.Join(errs, "\n"))
	}
	var doc struct {
		Results struct {
			Summary     struct{ Tests, Passed, Failed, Skipped int }
			Environment struct{ OSRelease string }
			Extra       struct{ Cells []struct{ JUnit string } }
		}
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if kernel, err := exec.Command("uname", "-r").Output(); err != nil || doc.Results.Environment.OSRelease != strings.TrimSpace(string(kernel)) {
		t.Errorf("osRelease %q, want uname -r's %q (%v)", doc.Results.Environment.OSRelease, kernel, err)
	}
	if got, want := doc.Results.Summary, (struct{ Tests, Passed, Failed, Skipped int }{10, 5, 3, 2}); got != want {
		t.Errorf("summary %+v, want %+v", got, want)
	}
	cases := 0
	for _, c := range doc.Results.Extra.Cells {
		var junit struct {
			Suite struct {
				Cases []struct{} `xml:"testcase"`
			} `xml:"testsuite"`
		}
		x, err := os.ReadFile(filepath.Join(rep.Dir, c.JUnit))
		if err == nil {
			err = xml.Unmarshal(x, &junit)
		}
		if err != nil {
			t.Fatalf("%s: %v", c.JUnit, err)
		}
		cases += len(junit.Suite.Cases)
	}
	if len(doc.Results.Extra.Cells) != 3 || cases != doc.Results.Summary.Tests {
		t.Errorf("%d JUnit files with %d testcases, for 3 cells and %d tests", len(doc.Results.Extra.Cells), cases, doc.Results.Summary.Tests)
	}
}
