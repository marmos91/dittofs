package run

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/marmos91/dittofs/test/harness/plan"
	"github.com/marmos91/dittofs/test/harness/registry"
)

// Cell is a planned cell with the suite or option it runs and the tier it
// runs at.
type Cell struct {
	plan.Cell
	Unit *registry.Suite
	Tier string
}

// Select returns the cells `dt run TARGET` runs. TARGET is a group, a suite,
// or a suite's cells by the first segments of their names; option picks one
// of the suite's options. Each suite or option runs at tier, or at its own
// tier when that is wider, and a TARGET that names values keeps every matrix
// value. only and skip narrow a group; tests, when given, are the tests to run,
// and each must name one. A group whose suites are all skipped gives none:
// nothing to run.
func Select(root string, r *registry.Registry, target, option, tier string, only, skip, tests []string) ([]Cell, error) {
	if !slices.Contains(registry.Tiers, tier) {
		return nil, fmt.Errorf("tier %q is not one of %s", tier, strings.Join(registry.Tiers, ", "))
	}
	name, rest, _ := strings.Cut(target, "/")
	suites, err := suitesOf(r, name, only, skip)
	if err != nil {
		return nil, err
	}
	if len(suites) > 1 && (rest != "" || option != "") {
		return nil, fmt.Errorf("%s is a group: name one of its suites to pick cells or an option", name)
	}
	var cells []Cell
	found := map[string]bool{}
	for _, s := range suites {
		unit, path, err := unitOf(r, s, option, rest)
		if err != nil {
			return nil, err
		}
		goTests := unit.Format == "go-json" // go test finds its tests; TEST names them
		if len(tests) > 0 && !unit.Tests.Set() && !goTests {
			if len(suites) > 1 {
				continue // a group's suites without tests have none to pick
			}
			return nil, fmt.Errorf("%s has no tests to choose from", unit.Name)
		}
		at := wider(tier, unit.Tier)
		planned, err := plan.UnitCells(root, unit, at, path != "")
		if err != nil {
			return nil, fmt.Errorf("%s: %w", unit.Name, err)
		}
		want := unit.Name + "/" + path
		for _, c := range planned {
			if path != "" && c.Name != want && !strings.HasPrefix(c.Name, want+"/") {
				continue
			}
			cell := Cell{Cell: c, Unit: unit, Tier: at}
			switch {
			case len(tests) > 1 && goTests && slices.ContainsFunc(tests, func(n string) bool { return strings.Contains(n, "/") }):
				// One -run pattern for several names would filter every
				// matched test's subtests by each subtest name.
				return nil, errors.New("name subtests one at a time")
			case len(tests) > 0 && (goTests || unit.Tests.Deferred()):
				// Only the running cell finds these tests, and it reports a
				// name that finds none.
				cell.Tests = tests
				for _, name := range tests {
					found[name] = true
				}
			case len(tests) > 0:
				if cell.Tests, err = chosen(root, cell, tests, found); err != nil {
					return nil, err
				}
				if len(cell.Tests) == 0 {
					continue
				}
			}
			cells = append(cells, cell)
		}
	}
	for _, name := range tests {
		if !found[name] {
			return nil, fmt.Errorf("no test %q in %s", name, target)
		}
	}
	if len(cells) == 0 && rest != "" {
		return nil, fmt.Errorf("no cell matches %s", target)
	}
	return cells, nil
}

// suitesOf returns the suites name stands for: a group's, narrowed by only
// and skip, or the one suite.
func suitesOf(r *registry.Registry, name string, only, skip []string) ([]string, error) {
	group, isGroup := r.Groups[name]
	if !isGroup {
		if r.Suite(name) == nil {
			return nil, fmt.Errorf("no suite or group %q", name)
		}
		if len(only)+len(skip) > 0 {
			return nil, fmt.Errorf("--only and --skip narrow a group, and %s is a suite", name)
		}
		return []string{name}, nil
	}
	for _, s := range append(slices.Clone(only), skip...) {
		if !slices.Contains(group, s) {
			return nil, fmt.Errorf("group %s has no suite %q", name, s)
		}
	}
	var out []string
	for _, s := range group {
		if (len(only) == 0 || slices.Contains(only, s)) && !slices.Contains(skip, s) {
			out = append(out, s)
		}
	}
	return out, nil
}

// unitOf returns the suite, or the option that option or the path's first
// segment names, and what remains of the path.
func unitOf(r *registry.Registry, suite, option, path string) (*registry.Suite, string, error) {
	units := r.Units(r.Suite(suite))
	find := func(name string) *registry.Suite {
		i := slices.IndexFunc(units, func(u *registry.Suite) bool { return u.Name == suite+"/"+name })
		if i < 0 {
			return nil
		}
		return units[i]
	}
	if option != "" {
		if u := find(option); u != nil {
			return u, path, nil
		}
		return nil, "", fmt.Errorf("%s has no option %q", suite, option)
	}
	first, rest, _ := strings.Cut(path, "/")
	if u := find(first); first != "" && u != nil {
		return u, rest, nil
	}
	return units[0], path, nil
}

// wider returns the wider of two tiers.
func wider(a, b string) string {
	if slices.Index(registry.Tiers, b) > slices.Index(registry.Tiers, a) {
		return b
	}
	return a
}

// chosen returns the cell's tests that the names pick, by path or file name,
// and marks each name that picked one in found.
func chosen(root string, c Cell, names []string, found map[string]bool) ([]string, error) {
	tests := c.Tests
	if len(tests) == 0 {
		all, _, err := plan.ListTests(root, c.Unit, "")
		if err != nil {
			return nil, err
		}
		tests = all
	}
	var out []string
	for _, t := range tests {
		picked := false
		for _, name := range names {
			if picks(name, t) {
				found[name] = true
				picked = true
			}
		}
		if picked {
			out = append(out, t)
		}
	}
	return out, nil
}

// picks reports whether a name given on the command line picks the test: it
// is its name or its file name.
func picks(name, test string) bool {
	return name == test || name == filepath.Base(test)
}

// deferredTests finds the tests of a glob holding a placeholder, once the
// running cell has filled it, and maps each to its file. A test is named by
// its path below the glob's last directory without a wildcard, as the known
// lists name pjdfstest's chmod/00.t below {tools}/pjdfstest/tests. names,
// when given, pick tests by name or file name, and each must pick one.
func deferredTests(root string, u *registry.Suite, vals map[string]string, lists map[string][]string, names []string) ([]string, map[string]string, error) {
	dir, err := replace(globDir(u.Tests.Glob), vals, lists, nil)
	if err != nil {
		return nil, nil, err
	}
	glob, err := replace(u.Tests.Glob, vals, lists, nil)
	if err != nil {
		return nil, nil, err
	}
	if !filepath.IsAbs(glob) {
		dir, glob = filepath.Join(root, dir), filepath.Join(root, glob)
	}
	all, err := plan.Glob(dir, glob, u.Exclude)
	if err != nil {
		return nil, nil, err
	}
	for _, name := range names {
		if !slices.ContainsFunc(all, func(t string) bool { return picks(name, t) }) {
			return nil, nil, fmt.Errorf("no test %q in %s", name, u.Tests.Glob)
		}
	}
	var tests []string
	paths := map[string]string{}
	for _, t := range all {
		if len(names) == 0 || slices.ContainsFunc(names, func(name string) bool { return picks(name, t) }) {
			tests = append(tests, t)
			paths[t] = filepath.Join(dir, filepath.FromSlash(t))
		}
	}
	return tests, paths, nil
}

// globDir is a glob's path up to its first element with a wildcard.
func globDir(glob string) string {
	elems := strings.Split(glob, "/")
	i := slices.IndexFunc(elems, func(e string) bool { return strings.ContainsAny(e, `*?[`) })
	if i < 0 {
		i = len(elems) - 1
	}
	return strings.Join(elems[:i], "/")
}
