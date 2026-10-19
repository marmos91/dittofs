// Package plan expands the registry into the cells a tier runs, each with the
// place it runs on (docs/internals/rfcs/rfc-29-test-harness.md).
package plan

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/marmos91/dittofs/test/harness/registry"
)

// Places a cell can run on.
const (
	ubuntu    = "ubuntu"
	windows   = "windows"
	macOS     = "macos"
	dedicated = "dedicated"
)

// runnerDiskGB is the disk a shared place offers; a cell needing more goes to
// the dedicated host.
const runnerDiskGB = 14

// FixtureSetup is the limit of a fixture's own steps, which a cell's limit
// always covers.
const FixtureSetup = 5 * time.Minute

// Cell is one unit of work: a suite or option, at one point of its matrix,
// with some or all of its tests, on one place.
type Cell struct {
	Name     string            `json:"name"`
	Suite    string            `json:"suite"`
	Option   string            `json:"option,omitempty"`
	Matrix   map[string]string `json:"matrix,omitempty"`
	Tests    []string          `json:"tests,omitempty"` // when the cell runs part of the suite's tests
	Place    string            `json:"place"`
	Needs    []string          `json:"needs,omitempty"`
	Blocking bool              `json:"blocking"`
	Job      string            `json:"job,omitempty"`
	// TestTimeout is each test's limit; "size" takes it from a scenario's size class.
	TestTimeout string `json:"test_timeout"`
	// TimeoutMinutes is the whole cell's limit, fixture included.
	TimeoutMinutes int      `json:"timeout_minutes"`
	Notes          []string `json:"notes,omitempty"`
}

// Plan returns the cells of tier, in registry order. root is the repository
// root, where the suites' test globs are expanded. changed lists the paths a
// pull request changes; nil means unknown, and plans every cell. On the pr
// tier a docs-only change plans nothing, and a suite with paths runs only
// when the change touches one of them.
func Plan(root string, r *registry.Registry, tier string, changed []string) ([]Cell, error) {
	if !slices.Contains(registry.Tiers, tier) {
		return nil, fmt.Errorf("tier %q is not one of %s", tier, strings.Join(registry.Tiers, ", "))
	}
	if tier != "pr" {
		changed = nil
	}
	if changed != nil && !slices.ContainsFunc(changed, func(p string) bool { return !isDocs(p) }) {
		return nil, nil
	}
	var cells []Cell
	for _, s := range r.Suites {
		for _, u := range r.Units(s) {
			if !RunsIn(u.Tier, tier) || !touches(u.Paths, changed) {
				continue
			}
			c, err := UnitCells(root, u, tier, false)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", u.Name, err)
			}
			cells = append(cells, c...)
		}
	}
	return cells, nil
}

// RunsIn reports whether a suite of tier unit runs in tier want. Each tier
// contains the ones before it; a manual suite runs only when manual is asked.
func RunsIn(unit, want string) bool {
	if unit == "manual" {
		return want == "manual"
	}
	return slices.Index(registry.Tiers, unit) <= slices.Index(registry.Tiers, want)
}

// isDocs reports whether a changed path is documentation, which no suite
// tests.
func isDocs(p string) bool {
	switch filepath.Ext(p) {
	case ".md", ".png", ".jpg", ".svg":
		return true
	}
	base := filepath.Base(p)
	return strings.HasPrefix(p, "docs/") || base == "LICENSE" || base == ".gitignore"
}

// touches reports whether a change touches one of a suite's paths. A suite
// without paths, or a change that isn't known, always runs.
func touches(globs, changed []string) bool {
	if len(globs) == 0 || changed == nil {
		return true
	}
	for _, g := range globs {
		for _, p := range changed {
			if dir, tree := strings.CutSuffix(g, "/**"); tree && strings.HasPrefix(p, dir+"/") || match(g, p) {
				return true
			}
		}
	}
	return false
}

// group is a set of a unit's tests that run together, with the needs they add
// and the needs_by_test glob that sets them apart.
type group struct {
	label string
	glob  string
	tests []string
	needs []string
}

// UnitCells expands one suite or option into its cells, narrowed by its
// select for tier. allValues keeps every matrix value, for a run that names
// the values it wants.
func UnitCells(root string, u *registry.Suite, tier string, allValues bool) ([]Cell, error) {
	sel := u.Select[tier]
	values := sel.Values
	if allValues {
		values = nil
	}
	groups, lines, err := testGroups(root, u, sel.Glob)
	if err != nil {
		return nil, err
	}
	suite, option, _ := strings.Cut(u.Name, "/")
	// A cell lists its tests only when it runs part of the suite's.
	partial := len(groups) > 1 || sel.Glob != "" || u.Shards > 1
	var cells []Cell
	for _, p := range points(u.Matrix, values) {
		for _, g := range groups {
			needs := union(union(u.Needs, g.needs), fixtureNeeds(u.Fixture, p.asMap()))
			place := placeOf(needs)
			shards := 1
			if u.Shards > 1 && place != dedicated {
				shards = u.Shards
			}
			for i := 1; i <= shards; i++ {
				segments := append([]string{u.Name}, p.values()...)
				if g.label != "" {
					segments = append(segments, g.label)
				}
				tests := g.tests
				if shards > 1 {
					tests = shard(g.tests, i, shards)
					// A cell with no tests listed runs the whole suite, so a
					// shard left empty by fewer tests than shards is dropped.
					if len(tests) == 0 {
						continue
					}
					segments = append(segments, "shard-"+strconv.Itoa(i))
				}
				c := Cell{
					Name: strings.Join(segments, "/"), Suite: suite, Option: option,
					Matrix: p.asMap(), Place: place, Needs: needs,
					Blocking: blocking(u.Blocking, tier), Job: u.Job, TestTimeout: u.Timeout.Test,
				}
				if partial {
					c.Tests = tests
				}
				if err := c.setTimeout(u.Timeout, tests, lines); err != nil {
					return nil, err
				}
				cells = append(cells, c)
			}
		}
	}
	return cells, nil
}

// fixtureNeeds are the needs fixture f implies at one point of its matrix,
// for its own steps: a service for an S3 profile, root and the client for a
// mount, and root and the port for SMB on a set port.
func fixtureNeeds(f *registry.Fixture, values map[string]string) []string {
	if f == nil {
		return nil
	}
	fill := func(s string) string {
		return registry.PlaceholderRE.ReplaceAllStringFunc(s, func(m string) string {
			if v, ok := values[m[1:len(m)-1]]; ok {
				return v
			}
			return m
		})
	}
	var out []string
	if fill(f.Profile) == "badger-s3" {
		out = append(out, "service:localstack")
	}
	if v := fill(f.NFS.Version); v != "" && v != "export" {
		out = append(out, "root", "nfs-client")
	}
	if f.SMB.Mount {
		out = union(out, []string{"root", "smb-client"})
	}
	if f.SMB.Port != 0 {
		out = union(out, []string{"root", "port:" + strconv.Itoa(f.SMB.Port)})
	}
	return out
}

// NeedsByTest gives, per needs_by_test glob, the needs it adds, for the globs
// that match a test tier plans. Without a tier, no select narrows the tests.
func NeedsByTest(root string, u *registry.Suite, tier string) (map[string][]string, error) {
	groups, _, err := testGroups(root, u, u.Select[tier].Glob)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, g := range groups {
		if g.glob != "" {
			out[g.glob] = g.needs
		}
	}
	return out, nil
}

// testGroups splits the unit's tests into the ones its needs cover and, per
// needs_by_test entry, the ones that need more, and returns what a .txt
// list's lines give its tests. Tests are listed only when something needs
// them: a split, shards, a narrowing glob, size timeouts or a list.
func testGroups(root string, u *registry.Suite, narrow string) ([]group, map[string]Line, error) {
	if len(u.NeedsByTest) == 0 && u.Shards <= 1 && narrow == "" && u.Timeout.Test != "size" && u.Tests.File == "" {
		return []group{{}}, nil, nil
	}
	tests, lines, err := ListTests(root, u, narrow)
	if err != nil {
		return nil, nil, err
	}
	// Planning needs the list here, so an empty one is a registry mistake (a
	// glob or a tier's select matching nothing, or a glob holding a
	// placeholder, which only a running cell fills) rather than a suite to
	// drop.
	if len(tests) == 0 {
		src := u.Tests.Glob + u.Tests.File + strings.Join(u.Tests.Names, " ")
		if narrow != "" {
			src += " narrowed by " + narrow
		}
		return nil, nil, fmt.Errorf("tests %s list no test to plan", src)
	}
	globs := make([]string, 0, len(u.NeedsByTest))
	for g := range u.NeedsByTest {
		globs = append(globs, g)
	}
	slices.Sort(globs)
	base := group{}
	extra := map[string]*group{}
	for _, t := range tests {
		i := slices.IndexFunc(globs, func(g string) bool { return match(g, t) })
		if i < 0 {
			base.tests = append(base.tests, t)
			continue
		}
		g := extra[globs[i]]
		if g == nil {
			needs := u.NeedsByTest[globs[i]]
			g = &group{label: placeOf(union(u.Needs, needs)), glob: globs[i], needs: needs}
			extra[globs[i]] = g
		}
		g.tests = append(g.tests, t)
	}
	var out []group
	if len(base.tests) > 0 {
		out = append(out, base)
	}
	for _, glob := range globs {
		if g := extra[glob]; g != nil {
			out = append(out, *g)
		}
	}
	return out, lines, nil
}

// Line is what a .txt list's line gives its test besides its name.
type Line struct {
	Limit  time.Duration // its own limit; 0 when the line gives none
	Share  string        // share=: the share it runs on, in place of the fixture's first
	Prefix string        // prefix=: what the names the tool reports for it leave off
}

// ListTests expands the unit's tests: a glob relative to root, a list of
// names, or a .txt file of names, each line a name and then, in any order, its
// own limit, share= and prefix=. A glob holding a placeholder is resolved only
// when the cell runs, so it lists nothing here.
func ListTests(root string, u *registry.Suite, narrow string) ([]string, map[string]Line, error) {
	var tests []string
	lines := map[string]Line{}
	switch {
	case u.Tests.Glob != "" && !u.Tests.Deferred():
		var err error
		if tests, err = Glob(root, filepath.Join(root, u.Tests.Glob), u.Exclude); err != nil {
			return nil, nil, err
		}
	case len(u.Tests.Names) > 0:
		tests = u.Tests.Names
	case u.Tests.File != "":
		data, err := os.ReadFile(filepath.Join(root, u.Tests.File))
		if err != nil {
			return nil, nil, err
		}
		for i, text := range strings.Split(string(data), "\n") {
			fields := strings.Fields(text)
			if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
				continue
			}
			var l Line
			for _, f := range fields[1:] {
				key, value, isKey := strings.Cut(f, "=")
				limit, err := time.ParseDuration(f)
				switch {
				case isKey && key == "share":
					l.Share = value
				case isKey && key == "prefix":
					l.Prefix = value
				case err == nil && limit > 0 && l.Limit == 0:
					l.Limit = limit
				default:
					return nil, nil, fmt.Errorf("%s:%d: %q is not a limit above zero, share= or prefix=", u.Tests.File, i+1, f)
				}
			}
			tests = append(tests, fields[0])
			lines[fields[0]] = l
		}
	}
	if narrow != "" {
		tests = slices.DeleteFunc(slices.Clone(tests), func(t string) bool { return !match(narrow, t) })
	}
	return tests, lines, nil
}

// Glob returns the files pattern matches, each named by its path below dir
// with forward slashes, without those an exclude glob matches by path or
// file name.
func Glob(dir, pattern string, exclude []string) ([]string, error) {
	m, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	var tests []string
	for _, p := range m {
		rel, _ := filepath.Rel(dir, p)
		if !slices.ContainsFunc(exclude, func(x string) bool { return match(x, rel) }) {
			tests = append(tests, filepath.ToSlash(rel))
		}
	}
	return tests, nil
}

// match reports whether glob matches the test's path or its file name.
func match(glob, test string) bool {
	if ok, _ := filepath.Match(glob, test); ok {
		return true
	}
	ok, _ := filepath.Match(glob, filepath.Base(test))
	return ok
}

// shard returns the i-th of n slices of tests, dealt in turn, so long and
// short tests spread evenly.
func shard(tests []string, i, n int) []string {
	var out []string
	for j := i - 1; j < len(tests); j += n {
		out = append(out, tests[j])
	}
	return out
}

// point is one combination of matrix values, axes in file order.
type point []struct{ axis, value string }

func (p point) values() []string {
	out := make([]string, len(p))
	for i, v := range p {
		out[i] = v.value
	}
	return out
}

func (p point) asMap() map[string]string {
	if len(p) == 0 {
		return nil
	}
	m := make(map[string]string, len(p))
	for _, v := range p {
		m[v.axis] = v.value
	}
	return m
}

// points returns every combination of the matrix's values, each axis narrowed
// to the tier's selected values when it has some.
func points(m registry.Matrix, selected map[string][]string) []point {
	out := []point{nil}
	for _, a := range m {
		values := a.Values
		if s, ok := selected[a.Name]; ok {
			values = s
		}
		var next []point
		for _, p := range out {
			for _, v := range values {
				next = append(next, append(slices.Clone(p), struct{ axis, value string }{a.Name, v}))
			}
		}
		out = next
	}
	return out
}

// placeOf picks the place that meets the needs: the platform's own place, the
// dedicated host for what the shared places can't offer, and the default otherwise.
func placeOf(needs []string) string {
	for _, n := range needs {
		kind, arg, _ := strings.Cut(n, ":")
		switch {
		case n == "windows":
			return windows
		case n == "macos":
			return macOS
		case n == "host:dedicated":
			return dedicated
		case kind == "disk":
			if gb, _ := strconv.Atoi(arg); gb > runnerDiskGB {
				return dedicated
			}
		}
	}
	return ubuntu
}

func blocking(b registry.Blocking, tier string) bool {
	if v, ok := b.ByTier[tier]; ok {
		return v
	}
	if b.All != nil {
		return *b.All
	}
	return true
}

// setTimeout sets the cell's limits. A cell runs its tests one after
// another, so it never gets less than the sum of their limits plus the
// fixture's setup time; when that raises it, a note says so. A test's limit is
// its own from the list, else its size class's or the suite's, and a test
// with no bound counts as the cell's own limit.
func (c *Cell) setTimeout(t registry.Timeout, tests []string, lines map[string]Line) error {
	cell, err := time.ParseDuration(t.Cell)
	if err != nil {
		return err
	}
	// A suite whose tests aren't listed counts as one test.
	total := TestLimit(t, "", nil)
	if len(tests) > 0 {
		total = 0
		for _, name := range tests {
			limit := TestLimit(t, name, lines)
			if limit == 0 {
				limit = cell
			}
			total += limit
		}
	}
	if need := total + FixtureSetup; total > 0 && need > cell {
		// Rounded up to whole minutes, as TimeoutMinutes holds it, so the
		// note gives the limit the cell gets.
		cell = (need + time.Minute - 1).Truncate(time.Minute)
		c.Notes = append(c.Notes, fmt.Sprintf("cell timeout raised from %s to %s: the sum of its tests' limits plus %dm for the fixture", t.Cell, strings.TrimSuffix(cell.String(), "0s"), FixtureSetup/time.Minute))
	}
	c.TimeoutMinutes = int((cell + time.Minute - 1) / time.Minute)
	return nil
}

// TestLimit is a test's limit: its own from a .txt list, else its size
// class's or the suite's. Zero means no bound.
func TestLimit(t registry.Timeout, name string, lines map[string]Line) time.Duration {
	if limit := lines[name].Limit; limit > 0 {
		return limit
	}
	if t.Test == "size" {
		return sizeLimit(name)
	}
	limit, _ := time.ParseDuration(t.Test) // the registry checks it parses when it loads
	return limit
}

// sizeLimit is a scenario's limit from the size class in its file name: twice
// the class's bound plus 2 minutes of container setup. xxl has no bound.
func sizeLimit(name string) time.Duration {
	bounds := map[string]time.Duration{"xs": 10 * time.Second, "s": time.Minute, "m": 5 * time.Minute, "l": 15 * time.Minute, "xl": time.Hour}
	stem := strings.TrimSuffix(filepath.Base(name), filepath.Ext(name))
	class := stem[strings.LastIndex(stem, "-")+1:]
	if b, ok := bounds[class]; ok {
		return 2*b + 2*time.Minute
	}
	return 0
}

// union returns a followed by the items of b it lacks.
func union(a, b []string) []string {
	out := slices.Clone(a)
	for _, x := range b {
		if !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}
