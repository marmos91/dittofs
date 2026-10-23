// Command parity compares, cell by cell, the conformance suites' results
// through dt with their results through their scripts, which must match before
// the scripts go (docs/internals/rfcs/rfc-29-test-harness.md): the same
// verdict, the same new failures, the same known failures and the same number
// of tests run. It goes with the scripts.
//
//	go run ./test/conformance/parity --plan CELLS DIR SUITE...
//
// CELLS are the cells dt plan --json gave the run, as its JSON, and SUITE the
// suites whose scripts ran beside them. DIR holds the run's artifacts: dt's
// runs, each with its summary.json, and the scripts' cells, each in
// conformance/results/SUITE/LABEL with the runner's status and the grader's
// grades. It prints the comparison as Markdown, for the job summary, and exits
// 1 when a cell differs, as a planned cell missing on either side does, or when
// there is none to compare.
//
// The differences whose causes the RFC lists beforehand don't count:
//   - what dt grades and the scripts don't: dt's skips and tests with no
//     result aren't compared, a dt cell they alone leave not run is as green
//     as the scripts', and a cell whose scripts' grader stopped before grading
//     each test, as it does when one has no result, compares only its verdict,
//     and differs when the scripts pass it;
//   - where the server runs: a test whose difference comes from where its
//     server runs is named here, with the cause, before the runs it is
//     excused in. None is named yet.
package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/marmos91/dittofs/test/harness/report"
)

// side is a cell's results through dt or through the scripts.
type side struct {
	result string // how the cell went, in the side's words
	green  bool
	// excused says a dt cell is not run only for its skips or its tests with
	// no result, which the scripts don't grade.
	excused bool
	// graded is false when the scripts' grader stopped before grading each
	// test, as it does when one has no result: only the verdict compares.
	graded     bool
	new, known []string // the names of the new and the known failures, sorted, each once
	ran        int      // the tests that passed or failed, expected skips excluded
}

func main() {
	plan := flag.String("plan", "", "the cells dt plan --json gave, as JSON")
	flag.Parse()
	if *plan == "" || flag.NArg() < 2 {
		fmt.Fprintln(os.Stderr, "usage: parity --plan CELLS DIR SUITE...")
		os.Exit(2)
	}
	var cells []struct{ Name string }
	if err := json.Unmarshal([]byte(*plan), &cells); err != nil {
		fmt.Fprintln(os.Stderr, "parity: --plan:", err)
		os.Exit(2)
	}
	planned := make([]string, len(cells))
	for i, c := range cells {
		planned[i] = c.Name
	}
	text, same, err := compare(flag.Arg(0), planned, flag.Args()[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "parity:", err)
		os.Exit(2)
	}
	fmt.Print(text)
	if !same {
		os.Exit(1)
	}
}

// compare compares the planned cells, and the scripts' cells of suites, in
// dir, and says whether they are all the same. A cell missing on either side
// differs, and comparing none isn't the same.
func compare(dir string, planned, suites []string) (string, bool, error) {
	dt := map[string]side{}
	scripts := map[string]side{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		p := strings.Split(filepath.ToSlash(path), "/")
		switch {
		case d.Name() == "summary.json":
			r, err := report.Read(filepath.Dir(path))
			if err != nil {
				return err
			}
			for _, c := range r.Cells {
				dt[c.Name] = fromDT(c)
			}
		case d.Name() == "status" && len(p) >= 5 && p[len(p)-5] == "conformance" && p[len(p)-4] == "results" && slices.Contains(suites, p[len(p)-3]):
			s, err := fromScripts(filepath.Dir(path))
			if err != nil {
				return err
			}
			scripts[p[len(p)-3]+"/"+p[len(p)-2]] = s
		}
		return nil
	})
	if err != nil {
		return "", false, err
	}

	var b strings.Builder
	b.WriteString("### dt and the scripts, cell by cell\n\n")
	cells, differ := 0, 0
	line := func(name, results string, diffs []string) {
		cells++
		verdict := "the same"
		if len(diffs) > 0 {
			differ++
			verdict = "differs: " + strings.Join(diffs, "; ")
		}
		_, _ = fmt.Fprintf(&b, "- `%s` (%s): %s\n", name, results, verdict)
	}
	paired := map[string]bool{}
	for _, name := range planned {
		d, inDT := dt[name]
		key := scriptsName(name)
		s, inScripts := scripts[key]
		paired[key] = true
		results := "dt: " + cmp.Or(d.result, "none")
		if inScripts {
			results += ", scripts: " + s.result
		}
		var diffs []string
		switch {
		case !inDT:
			diffs = []string{"no results through dt"}
		case !inScripts:
			diffs = []string{"no results through the scripts"}
		default:
			diffs = differences(d, s)
		}
		line(name, results, diffs)
	}
	for _, key := range slices.Sorted(maps.Keys(scripts)) {
		if !paired[key] {
			line(key, "scripts: "+scripts[key].result, []string{"no results through dt"})
		}
	}
	if cells == 0 {
		b.WriteString("No cells to compare.\n")
		return b.String(), false, nil
	}
	_, _ = fmt.Fprintf(&b, "\n%d of %d cells differ.\n", differ, cells)
	return b.String(), differ == 0, nil
}

// scriptsName is the scripts' name for a dt cell: its suite, then its matrix
// values joined with "-", as the scripts' runner names its results
// directories.
func scriptsName(cell string) string {
	suite, values, _ := strings.Cut(cell, "/")
	return suite + "/" + strings.ReplaceAll(values, "/", "-")
}

// fromDT is a cell's results through dt. A test ran when the tool gave it a
// pass or a failure, whatever its grade made of it, but for an expected skip,
// which the scripts count as a skip.
func fromDT(c report.Cell) side {
	d := side{result: c.Result, green: c.Result == report.Passed, graded: true}
	onlyDT := false
	for _, t := range c.Tests {
		switch {
		case t.NewFailure():
			d.new = append(d.new, t.Name)
		case t.Grade == report.Known:
			d.known = append(d.known, t.Name)
		case t.Grade == report.UnexpectedSkip || t.Grade == report.Incomplete:
			onlyDT = true
		}
		if raw := cmp.Or(t.RawStatus, t.Status); t.Grade != report.ExpectedSkip && (raw == report.Passed || raw == report.Failed) {
			d.ran++
		}
	}
	// Not run for one of what only dt grades, a cell is green as far as the
	// scripts grade. A failed one isn't: it has a new failure, an unexpected
	// pass, or a server that exited.
	d.excused = c.Result != report.Passed && c.Result != report.Failed && onlyDT
	d.new, d.known = set(d.new), set(d.known)
	return d
}

// fromScripts is a cell's results through the scripts, from the runner's exit
// status and the grader's grades in dir: a line "known NAME" or "new NAME" for
// each failure, then "ran N" once it graded every test.
func fromScripts(dir string) (side, error) {
	status, err := os.ReadFile(filepath.Join(dir, "status"))
	if err != nil {
		return side{}, err
	}
	code := strings.TrimSpace(string(status))
	s := side{result: "exit " + code, green: code == "0"}
	grades, err := os.ReadFile(filepath.Join(dir, "grades"))
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return side{}, err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(grades)), "\n") {
		word, value, _ := strings.Cut(strings.TrimSpace(line), " ")
		switch word {
		case "known":
			s.known = append(s.known, value)
		case "new":
			s.new = append(s.new, value)
		case "ran":
			if s.ran, err = strconv.Atoi(value); err != nil {
				return side{}, fmt.Errorf("%s: %w", filepath.Join(dir, "grades"), err)
			}
			s.graded = true
		default:
			return side{}, fmt.Errorf("%s: unknown line %q", filepath.Join(dir, "grades"), line)
		}
	}
	s.new, s.known = set(s.new), set(s.known)
	return s, nil
}

// differences says how two sides of a cell differ, but for what the RFC
// lists beforehand.
func differences(d, s side) []string {
	var out []string
	if d.green != s.green && (!s.green || !d.excused) {
		out = append(out, "the verdict")
	}
	if !s.graded {
		if s.green {
			out = append(out, "the scripts passed it without grading each test")
		}
		return out
	}
	for _, f := range []struct {
		what        string
		dt, scripts []string
	}{{"new failures", d.new, s.new}, {"known failures", d.known, s.known}} {
		if !slices.Equal(f.dt, f.scripts) {
			out = append(out, fmt.Sprintf("%s only through dt: %s; only through the scripts: %s", f.what, list(without(f.dt, f.scripts)), list(without(f.scripts, f.dt))))
		}
	}
	if d.ran != s.ran {
		out = append(out, fmt.Sprintf("tests run: %d through dt, %d through the scripts", d.ran, s.ran))
	}
	return out
}

// without is a, less what b holds.
func without(a, b []string) []string {
	return slices.DeleteFunc(slices.Clone(a), func(x string) bool { return slices.Contains(b, x) })
}

func list(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// set is names sorted, each once.
func set(names []string) []string {
	slices.Sort(names)
	return slices.Compact(names)
}
