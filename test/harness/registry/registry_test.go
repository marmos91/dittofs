package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/test/harness/needs"
)

// Every need the registry accepts says how to meet it, for dt doctor.
func TestEveryNeedHasAHint(t *testing.T) {
	all := []string{"disk:1", "port:1", "service:kmip", "cloud:aws"}
	for n := range plainNeeds {
		all = append(all, n)
	}
	for _, n := range all {
		if err := checkNeed(n); err != nil {
			t.Error(err)
		}
		if needs.Hint(n) == "" {
			t.Errorf("%s has no hint", n)
		}
	}
}

// The repository's own registry loads and passes every rule.
func TestRepositoryRegistryLoads(t *testing.T) {
	r, err := Load("../../..")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Suites) == 0 {
		t.Fatal("no suites")
	}
}

// Each rule the registry is loaded with refuses what it should.
func TestLoadRules(t *testing.T) {
	const ok = "version: 1\ndefaults: { format: exit, tier: merge, timeout: { test: 30m, cell: 60m } }\n"
	cases := []struct {
		name, yaml, want string
	}{
		{"names are [a-z0-9-]+", ok + "suites:\n  Bad_Name: { cmd: true }\n", "a name is"},
		{"unknown fields are refused", ok + "suites:\n  a: { cmd: true, tiers: pr }\n", `unknown field "tiers"`},
		{"unknown fields are refused at any depth", ok + "suites:\n  a: { cmd: true, fixture: { profil: memory } }\n", `unknown field "profil"`},
		{"a newer version is refused", "version: 2\n", "this dt reads version 1"},
		{"known paths exist", ok + "suites:\n  a: { cmd: true, known: missing.yaml }\n", "missing.yaml does not exist"},
		{"{test} needs tests", ok + "suites:\n  a: { cmd: 'run {test}' }\n", "has no tests"},
		{"groups name existing suites", ok + "groups: { g: [nope] }\nsuites:\n  a: { cmd: true }\n", `no suite "nope"`},
		{"commands aren't blank", ok + "suites:\n  a: { cmd: [\"true\", \"\"] }\n", "a command is blank"},
		{"go-json begins with go test", ok + "suites:\n  a: { cmd: make test, format: go-json }\n", "begins with go test"},
		{"every matrix axis is used", ok + "suites:\n  a: { cmd: true, matrix: { profile: [memory] } }\n", `axis "profile" is used by no command`},
		{"unknown placeholders are refused", ok + "suites:\n  a: { cmd: 'run {nope}' }\n", "unknown placeholder {nope}"},
		{"placeholders side by side are each checked", ok + "suites:\n  a: { cmd: 'run {out}{nope}' }\n", "unknown placeholder {nope}"},
		{"tiers are known", ok + "suites:\n  a: { cmd: true, tier: weekly }\n", `tier "weekly"`},
		{"an option isn't named like a dt run flag", ok + "suites:\n  a: { cmd: true, options: { ci: { cmd: false } } }\n", "dt run has a flag of that name"},
		{"needs are known", ok + "suites:\n  a: { cmd: true, needs: [linx] }\n", `unknown need "linx"`},
		{"services are known", ok + "suites:\n  a: { cmd: true, needs: [\"service:nosuch\"] }\n", `unknown need "service:nosuch"`},
		{"a matrix axis has values", ok + "suites:\n  a: { cmd: 'run {p}', matrix: { p: [] } }\n", `axis "p" has no values`},
		{"a selection names a value", ok + "suites:\n  a: { cmd: 'run {p}', matrix: { p: [x] }, select: { pr: { p: [] } } }\n", "selects no value"},
		{"select names matrix values", ok + "suites:\n  a: { cmd: 'run {p}', matrix: { p: [x] }, select: { pr: { p: [y] } } }\n", `has no value "y"`},
		{"{tools} names a tool", ok + "suites:\n  a: { cmd: 'run {tools}' }\n", "{tools} names no tool: write {tools}/NAME, NAME one of pjdfstest, pynfs"},
		{"{tools} names a tool dt has", ok + "suites:\n  a: { cmd: 'run {tools}/pjdfstests/x' }\n", "{tools}/pjdfstests: no such tool; {tools} holds pjdfstest, pynfs"},
		{"{tools} names no path out of it", ok + "suites:\n  a: { cmd: 'run {tools}/../x' }\n", "{tools}/..: no such tool"},
		{"a placeholder's tests aren't sharded", ok + "suites:\n  a: { cmd: 'run {test}', tests: '{tools}/t/*.t', shards: 2 }\n", "shards needs the tests when the cell is planned"},
		{"a placeholder's tests have no needs_by_test", ok + "suites:\n  a: { cmd: 'run {test}', tests: '{tools}/t/*.t', needs_by_test: { 'a*': [linux] } }\n", "needs_by_test needs the tests"},
		{"a placeholder's tests aren't timed by size", ok + "suites:\n  a: { cmd: 'run {test}', tests: '{tools}/t/*.t', timeout: { test: size } }\n", "timeout.test: size needs the tests"},
		{"a placeholder's tests aren't narrowed by a glob", ok + "suites:\n  a: { cmd: 'run {test}', tests: '{tools}/t/*.t', select: { pr: 'a*' } }\n", "select.pr needs the tests"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse(t.TempDir(), []byte(c.yaml))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want an error containing %q", err, c.want)
			}
		})
	}
}

// A shell ${VAR} inside an sh command is the shell's, not a placeholder.
func TestShellVariablesAreNotPlaceholders(t *testing.T) {
	y := "version: 1\ndefaults: { format: exit, tier: merge, timeout: { test: 30m, cell: 60m } }\n" +
		"suites:\n  a: { cmd: { sh: 'echo ${HOME} {out}' } }\n"
	if _, err := Parse(t.TempDir(), []byte(y)); err != nil {
		t.Fatal(err)
	}
}

// A shell's ${tools}/x in an sh command isn't a test tool the suite names.
func TestShellVariableNamesNoTool(t *testing.T) {
	y := "version: 1\ndefaults: { format: exit, tier: merge, timeout: { test: 30m, cell: 60m } }\n" +
		"suites:\n  a: { cmd: { sh: 'echo ${tools}/x {tools}/pynfs/bin/pynfs-4.1' } }\n"
	r, err := Parse(t.TempDir(), []byte(y))
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Units(r.Suites[0])[0].Tools(); len(got) != 1 || got[0] != "pynfs" {
		t.Errorf("tools %q, want only pynfs", got)
	}
}

// A template takes the placeholders a command takes, a matrix axis's
// included, and leaves a ${VAR} alone; one it doesn't know is a registry
// error, as in a command.
func TestTemplatePlaceholders(t *testing.T) {
	root := t.TempDir()
	for name, body := range map[string]string{
		"ok/a.template":  "{server} {user2} {password} {p} ${KEEP}\n",
		"bad/a.template": "{server}\n",
		"bad/b.template": "{nope}\n",
	} {
		if err := os.MkdirAll(filepath.Join(root, filepath.Dir(name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	y := "version: 1\ndefaults: { format: exit, tier: merge, timeout: { test: 30m, cell: 60m } }\n" +
		"suites:\n  a: { image: x, templates: 'DIR/*.template', matrix: { p: [v] }, env: { P: '{p}' } }\n"
	if _, err := Parse(root, []byte(strings.Replace(y, "DIR", "ok", 1))); err != nil {
		t.Errorf("ok: %v", err)
	}
	_, err := Parse(root, []byte(strings.Replace(y, "DIR", "bad", 1)))
	if err == nil || err.Error() != "a: template b.template: unknown placeholder {nope}" {
		t.Errorf("bad: got %v", err)
	}
}

// Paths the registry names are found relative to the repository root.
func TestKnownPathRelativeToRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "known.yaml"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	y := "version: 1\ndefaults: { format: exit, tier: merge, timeout: { test: 30m, cell: 60m } }\n" +
		"suites:\n  a: { cmd: true, known: known.yaml }\n"
	if _, err := Parse(root, []byte(y)); err != nil {
		t.Fatal(err)
	}
}

// An option replaces each field it sets as a whole; the two timeout keys are
// set separately; an option with its own matrix drops the suite's select.
func TestOptionsReplaceFields(t *testing.T) {
	y := `version: 1
defaults: { format: exit, tier: merge, timeout: { test: 30m, cell: 60m } }
suites:
  s:
    cmd: run {profile}
    job: Fixed Job
    needs: [linux, docker]
    env: { A: "1" }
    timeout: { cell: 90m }
    matrix: { profile: [memory, badger] }
    select: { pr: { profile: [badger] } }
    options:
      o:
        cmd: run once
        needs: [windows]
        timeout: { test: 5m }
        matrix: {}
`
	r, err := Parse(t.TempDir(), []byte(y))
	if err != nil {
		t.Fatal(err)
	}
	units := r.Units(r.Suite("s"))
	base, opt := units[0], units[1]
	if opt.Name != "s/o" {
		t.Errorf("option name %q, want s/o", opt.Name)
	}
	if got := strings.Join(opt.Needs, ","); got != "windows" {
		t.Errorf("needs %q: an option's needs replace the suite's", got)
	}
	if opt.Env.All["A"] != "1" {
		t.Error("env the option doesn't set is the suite's")
	}
	if opt.Timeout.Test != "5m" || opt.Timeout.Cell != "90m" {
		t.Errorf("timeout %+v, want test from the option and cell from the suite", opt.Timeout)
	}
	if opt.Matrix == nil || len(opt.Matrix) != 0 {
		t.Errorf("matrix %v: an empty matrix replaces the suite's", opt.Matrix)
	}
	if opt.Select != nil {
		t.Error("an option with its own matrix keeps the suite's select")
	}
	if opt.Job != "" || base.Job != "Fixed Job" {
		t.Errorf("job: suite %q, option %q; only the suite has it", base.Job, opt.Job)
	}
	if base.Format != "exit" || base.Tier != "merge" || base.Timeout.Test != "30m" {
		t.Errorf("defaults not applied: %+v", base)
	}
}
