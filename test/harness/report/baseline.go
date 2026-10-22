package report

import (
	"cmp"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// smbtortureVersion is the line smbtorture starts its output with.
var smbtortureVersion = regexp.MustCompile(`(?m)^smbtorture [^\r\n]*`)

// failures matches a part of a cell's note that counts its failed tests, as
// dt run writes it: new failures or unexpected passes.
var failures = regexp.MustCompile(`^\d+ (failed|passed unexpectedly): `)

// Baseline is the results of the cell named cell, in the run in dir, as
// smbtorture's baseline file: a snapshot of them, which grades nothing. A
// sharded cell's results are its shards', cell/shard-N, together. Each result
// counts as smbtorture's scripts counted it: a failure, known or new, by its
// grade, and a quarantined one as known; a pass, quarantined or not, by the
// tool's own status, so an unexpected pass is a pass; and anything else as a
// skip, an expected skip and the draw its reader excuses included. A
// sub-suite is a name's first two dot-separated parts, and suites and names
// sort byte by byte, so the file is the same on every machine. smbtorture's
// version comes from the cell's log, the first shard's that has it.
//
// A run of a checkout with changes is an error, since its commit doesn't say
// what ran. So is a cell or shard that didn't run to its end, whose lost tests
// would leave the file without a trace: one whose note gives any reason but
// its tests' failures, as a cell not run always does, such as a limit that
// stopped it, a crash or a test that didn't run.
func Baseline(dir, cell string) ([]byte, error) {
	r, err := Read(dir)
	if err != nil {
		return nil, err
	}
	if r.Dirty {
		return nil, fmt.Errorf("run %s ran with changes its commit %s doesn't have", r.Name, r.Commit)
	}
	cells := slices.DeleteFunc(slices.Clone(r.Cells), func(c Cell) bool {
		return c.Name != cell && !strings.HasPrefix(c.Name, cell+"/shard-")
	})
	if len(cells) == 0 {
		return nil, fmt.Errorf("run %s has no cell %s", r.Name, cell)
	}
	var tests []Test
	version := ""
	for _, c := range cells {
		for part := range strings.SplitSeq(c.Note, "; ") {
			if part != "" && !failures.MatchString(part) {
				return nil, fmt.Errorf("%s didn't run to its end in run %s: %s: %s", c.Name, r.Name, c.Result, c.Note)
			}
		}
		if log, err := os.ReadFile(filepath.Join(dir, c.Log)); err == nil && version == "" {
			version = smbtortureVersion.FindString(string(log))
		}
		tests = append(tests, c.Tests...)
	}
	version = cmp.Or(version, "unknown")

	const pass, fail, skip = 0, 1, 2
	var count [3]int
	knownFails, newFails := 0, 0
	groups := map[string]*[3][]string{} // each sub-suite's lines, by class
	for _, t := range tests {
		class, line := skip, t.Name
		switch {
		case t.Grade == New:
			class, line = fail, t.Name+" **(NEW)**"
			newFails++
		case t.Grade == Known, t.Grade == Quarantined && t.RawStatus == Failed:
			class, line = fail, t.Name+" _(known)_"
			knownFails++
		case cmp.Or(t.RawStatus, t.Status) == Passed:
			class = pass
		}
		parts := strings.SplitN(t.Name, ".", 3)
		sub := strings.Join(parts[:min(2, len(parts))], ".")
		if groups[sub] == nil {
			groups[sub] = &[3][]string{}
		}
		groups[sub][class] = append(groups[sub][class], line)
		count[class]++
	}
	rate := func(n, of int, format string) string {
		if of == 0 {
			return "N/A"
		}
		return fmt.Sprintf(format, float64(n)/float64(of)*100)
	}
	subs := slices.Sorted(maps.Keys(groups))

	var b strings.Builder
	fmt.Fprintf(&b, "# smbtorture Baseline Results\n\n"+
		"> Written by `dt known baseline` from one cell's results in a run. Don't edit it by hand:\n"+
		"> write it again, and commit it through a pull request. It grades nothing: a run is graded\n"+
		"> against the suite's `known.yaml`.\n\n"+
		"**Date:** %s\n**DittoFS Commit:** %s\n**Cell:** %s\n**Platform:** %s\n**smbtorture:** %s\n\n",
		r.Start.UTC().Format(time.DateOnly), r.Commit, cell, r.OS, version)
	fmt.Fprintf(&b, "## Overall Summary\n\n| Metric | Count |\n|--------|-------|\n"+
		"| Total Tests | %d |\n| Passed | %d |\n| Failed | %d |\n| Known failures | %d |\n"+
		"| New failures | %d |\n| Skipped | %d |\n| Pass Rate | %s |\n\n",
		len(tests), count[pass], count[fail], knownFails, newFails, count[skip], rate(count[pass], len(tests), "%.1f%%"))
	b.WriteString("## Per-Sub-Suite Breakdown\n\n| Sub-Suite | Pass | Fail | Skip | Total | Pass Rate |\n" +
		"|-----------|------|------|------|-------|-----------|\n")
	for _, s := range subs {
		g := groups[s]
		p, f, k := len(g[pass]), len(g[fail]), len(g[skip])
		fmt.Fprintf(&b, "| %s | %d | %d | %d | %d | %s |\n", s, p, f, k, p+f+k, rate(p, p+f, "%.0f%%"))
	}
	for _, sec := range []struct {
		title string
		class int
	}{{"Passing Tests", pass}, {"Failing Tests", fail}, {"Skipped Tests", skip}} {
		n := count[sec.class]
		fmt.Fprintf(&b, "\n## %s (%d)\n\n<details><summary>Show %d test(s)</summary>\n\n", sec.title, n, n)
		for _, s := range subs {
			if lines := groups[s][sec.class]; len(lines) > 0 {
				fmt.Fprintf(&b, "### %s (%d)\n- %s\n\n", s, len(lines), strings.Join(slices.Sorted(slices.Values(lines)), "\n- "))
			}
		}
		b.WriteString("</details>\n")
	}
	return []byte(b.String()), nil
}
