package registry

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/marmos91/dittofs/test/harness/needs"
)

// Tiers in order: each contains the ones before it. manual never runs on its own.
var Tiers = []string{"pr", "merge", "nightly", "manual"}

var (
	formats    = set("exit", "go-json", "junit", "tap", "subunit", "trx")
	metrics    = set("fio-json", "dfsbench-json")
	plainNeeds = set("linux", "windows", "macos", "root", "docker", "podman", "rpcbind", "knfsd", "nfs-client", "smb-client", "nfs4-acl-tools", "kerberos", "dm-flakey", "host:dedicated")
	// Placeholders every cell may use; a cell's matrix axes add their own.
	placeholders = set("test", "args", "base", "bin", "tools", "out", "packages", "server", "mount", "user", "password", "realm")

	// runFlags are dt run's own flags, which no option may share a name with.
	runFlags = set("ci", "tier", "only", "skip", "parallel", "retry", "in", "results", "help")

	nameRE = regexp.MustCompile(`^[a-z0-9-]+$`)
	// PlaceholderRE matches a {name}, or a shell's ${VAR}, which Placeholders
	// and dt run leave alone.
	PlaceholderRE = regexp.MustCompile(`\$?\{([A-Za-z0-9_.-]+)\}`)
	// toolRE matches where a suite names a test tool, as {tools}/pjdfstest.
	toolRE = regexp.MustCompile(`\{tools\}/([A-Za-z0-9_.-]+)`)
)

func set(items ...string) map[string]bool {
	m := map[string]bool{}
	for _, i := range items {
		m[i] = true
	}
	return m
}

func isTier(s string) bool { return set(Tiers...)[s] }

// check applies the rules the registry is loaded with.
func (r *Registry) check(root string) []error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if r.Version < 1 || r.Version > Version {
		fail("version %d: this dt reads version %d", r.Version, Version)
	}
	seen := map[string]bool{}
	for _, s := range r.Suites {
		if !nameRE.MatchString(s.Name) {
			fail("suite %q: a name is [a-z0-9-]+", s.Name)
		}
		if seen[s.Name] {
			fail("suite %q: defined twice", s.Name)
		}
		seen[s.Name] = true
		for _, o := range s.Options {
			if !nameRE.MatchString(o.Name) {
				fail("option %s/%s: a name is [a-z0-9-]+", s.Name, o.Name)
			}
			if runFlags[o.Name] {
				fail("option %s/%s: dt run has a flag of that name, and an option is a flag of its own", s.Name, o.Name)
			}
			if len(o.Options) > 0 {
				fail("option %s/%s: an option has no options", s.Name, o.Name)
			}
		}
		for _, u := range r.Units(s) {
			for _, err := range checkUnit(root, u) {
				fail("%s: %v", u.Name, err)
			}
		}
	}
	for g, members := range r.Groups {
		if !nameRE.MatchString(g) {
			fail("group %q: a name is [a-z0-9-]+", g)
		}
		for _, m := range members {
			if !seen[m] {
				fail("group %s: no suite %q", g, m)
			}
		}
	}
	return errs
}

// checkUnit checks one suite or option, with the defaults applied.
func checkUnit(root string, u *Suite) []error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if !formats[u.Format] {
		fail("format %q is not one of exit, go-json, junit, tap, subunit, trx", u.Format)
	}
	if u.Metrics != "" && !metrics[u.Metrics] {
		fail("metrics %q is not fio-json or dfsbench-json", u.Metrics)
	}
	if !isTier(u.Tier) {
		fail("tier %q is not one of %s", u.Tier, strings.Join(Tiers, ", "))
	}
	if len(u.Cmd) == 0 && u.Image == "" {
		fail("cmd is required unless an image provides one")
	}
	for _, c := range u.Cmd {
		if strings.TrimSpace(c.Line+c.Sh) == "" {
			fail("a command is blank")
		}
	}
	if u.Format == "go-json" {
		for _, c := range u.Cmd {
			if f := strings.Fields(c.Line); len(f) < 2 || f[0] != "go" || f[1] != "test" {
				fail("a go-json command begins with go test: %q", c.Line+c.Sh)
			}
		}
	}
	if u.Timeout.Test != "size" {
		if _, err := time.ParseDuration(u.Timeout.Test); err != nil {
			fail("timeout.test %q: a duration or size", u.Timeout.Test)
		}
	}
	if _, err := time.ParseDuration(u.Timeout.Cell); err != nil {
		fail("timeout.cell %q: a duration", u.Timeout.Cell)
	}
	for _, n := range u.Needs {
		if err := checkNeed(n); err != nil {
			fail("%v", err)
		}
	}
	for glob, extra := range u.NeedsByTest {
		if _, err := filepath.Match(glob, ""); err != nil {
			fail("needs_by_test %q: %v", glob, err)
		}
		for _, n := range extra {
			if err := checkNeed(n); err != nil {
				fail("needs_by_test %q: %v", glob, err)
			}
		}
	}
	for _, m := range []map[string]bool{tierKeys(u.Select), tierKeys(u.Args), tierKeys(u.Env.ByTier), tierKeys(u.Blocking.ByTier)} {
		for t := range m {
			if !isTier(t) {
				fail("%q is not a tier", t)
			}
		}
	}
	for _, g := range u.Paths {
		if _, err := filepath.Match(strings.TrimSuffix(g, "/**"), ""); err != nil {
			fail("paths %q: %v", g, err)
		}
	}
	if u.Shards < 0 {
		fail("shards is at least 1")
	}

	// Paths the suite reads must exist.
	for _, p := range []string{u.Known, fixtureShares(u.Fixture)} {
		if p != "" {
			if _, err := os.Stat(filepath.Join(root, p)); err != nil {
				fail("%s does not exist", p)
			}
		}
	}
	if u.Templates != "" {
		if m, _ := filepath.Glob(filepath.Join(root, u.Templates)); len(m) == 0 {
			fail("templates %s matches no file", u.Templates)
		}
	}

	// Placeholders: each is known, {test} needs tests, and each axis is used.
	axes := map[string]bool{}
	for _, a := range u.Matrix {
		if !nameRE.MatchString(a.Name) {
			fail("matrix axis %q: a name is [a-z0-9-]+", a.Name)
		}
		if len(a.Values) == 0 {
			fail("matrix axis %q has no values", a.Name)
		}
		axes[a.Name] = true
	}
	used := map[string]bool{}
	for _, s := range u.allStrings() {
		for _, p := range Placeholders(s) {
			used[p] = true
			if !placeholders[p] && !axes[p] {
				fail("unknown placeholder {%s}", p)
			}
		}
	}
	if used["test"] && !u.Tests.Set() {
		fail("{test} is used, but the suite has no tests")
	}
	// Only a running cell finds the tests of a glob holding a placeholder, so
	// nothing that needs them when the cell is planned can have them.
	if u.Tests.Deferred() {
		planned := map[string]bool{"shards": u.Shards > 1, "needs_by_test": len(u.NeedsByTest) > 0, "timeout.test: size": u.Timeout.Test == "size"}
		for t, sel := range u.Select {
			planned["select."+t] = sel.Glob != ""
		}
		for _, field := range slices.Sorted(maps.Keys(planned)) {
			if planned[field] {
				fail("%s needs the tests when the cell is planned, but tests %s holds a placeholder, which only a running cell fills", field, u.Tests.Glob)
			}
		}
	}
	inCell := map[string]bool{}
	for _, s := range u.cellStrings() {
		for _, p := range Placeholders(s) {
			inCell[p] = true
		}
	}
	for _, a := range u.Matrix {
		if !inCell[a.Name] {
			fail("matrix axis %q is used by no command, fixture or env", a.Name)
		}
	}
	for t, sel := range u.Select {
		for axis, values := range sel.Values {
			if !axes[axis] {
				fail("select.%s: no matrix axis %q", t, axis)
			}
			if len(values) == 0 {
				fail("select.%s: %s selects no value", t, axis)
			}
			for _, v := range values {
				if !slices.Contains(u.Matrix.values(axis), v) {
					fail("select.%s: %s has no value %q", t, axis, v)
				}
			}
		}
	}
	return errs
}

// Placeholders returns the {name} placeholders in s, leaving out a shell
// ${VAR} in an sh command.
func Placeholders(s string) []string {
	var out []string
	for _, m := range PlaceholderRE.FindAllStringSubmatch(s, -1) {
		if !strings.HasPrefix(m[0], "$") {
			out = append(out, m[1])
		}
	}
	return out
}

// Tools returns the test tools the suite names, as {tools}/NAME in any of its
// fields, each once, sorted by name.
func (u *Suite) Tools() []string {
	var out []string
	for _, s := range u.allStrings() {
		for _, m := range toolRE.FindAllStringSubmatch(s, -1) {
			if !slices.Contains(out, m[1]) {
				out = append(out, m[1])
			}
		}
	}
	slices.Sort(out)
	return out
}

func checkNeed(n string) error {
	kind, arg, hasArg := strings.Cut(n, ":")
	switch {
	case !hasArg && plainNeeds[n], n == "host:dedicated":
		return nil
	case kind == "disk" || kind == "port":
		if _, err := strconv.Atoi(arg); err == nil {
			return nil
		}
	case kind == "service" && needs.IsService(arg):
		return nil
	case kind == "cloud" && arg != "":
		return nil
	}
	return fmt.Errorf("unknown need %q", n)
}

// allStrings returns every string the suite's fields hold.
func (u *Suite) allStrings() []string {
	out := u.cellStrings()
	out = append(out, u.Dir, u.Image, u.Templates, u.Tests.Glob, u.Tests.File)
	out = append(out, u.Tests.Names...)
	for k, v := range u.Volumes {
		out = append(out, k, v)
	}
	for _, v := range u.Args {
		out = append(out, v)
	}
	return out
}

// cellStrings returns the strings of the command, the fixture and the env:
// what a matrix axis has to appear in.
func (u *Suite) cellStrings() []string {
	var out []string
	for _, c := range u.Cmd {
		out = append(out, c.Line, c.Sh)
	}
	if f := u.Fixture; f != nil {
		out = append(out, f.Server, f.Profile, f.NFS.Version, f.NFS.Options, f.SMB.Encryption, f.Shares, f.Squash, f.Lease, f.Reset)
	}
	for _, v := range u.Env.All {
		out = append(out, v)
	}
	for _, vars := range u.Env.ByTier {
		for _, v := range vars {
			out = append(out, v)
		}
	}
	return out
}

func (m Matrix) values(axis string) []string {
	for _, a := range m {
		if a.Name == axis {
			return a.Values
		}
	}
	return nil
}

func fixtureShares(f *Fixture) string {
	if f == nil {
		return ""
	}
	return f.Shares
}

func tierKeys[V any](m map[string]V) map[string]bool {
	out := map[string]bool{}
	for k := range m {
		out[k] = true
	}
	return out
}
