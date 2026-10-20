package registry

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

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
		{"fixture users are self or UIDs from 1 to 65534", ok + "suites:\n  a: { cmd: true, fixture: { profile: memory, users: [1, 65534, self, 0, -1, 65535, me] } }\n", "users: 0 is not self or a UID from 1 to 65534\na: fixture users: -1 is not self or a UID from 1 to 65534\na: fixture users: 65535 is not self or a UID from 1 to 65534\na: fixture users: me is not"},
		{"a fixture's client is a namespace", ok + "suites:\n  a: { cmd: true, fixture: { profile: memory, client: netns } }\n", `fixture client: "netns" is not namespace`},
		{"a second client needs the first namespaced", ok + "suites:\n  a: { cmd: true, fixture: { profile: memory, clients: 2 } }\n", "fixture clients: 2 adds a second client beside the namespaced one"},
		{"clients is 2", ok + "suites:\n  a: { cmd: true, fixture: { profile: memory, client: namespace, clients: 3 } }\n", "fixture clients: 2 adds"},
		{"rpcbind is the host's", ok + "suites:\n  a: { cmd: true, fixture: { profile: memory, rpcbind: own } }\n", `fixture rpcbind: "own" is not host`},
		{"a named value fills what the others do", ok + "suites:\n  a: { cmd: 'run {v} {w}', matrix: { s: [x: { v: 1, w: 2 }, y: { v: 3 }] } }\n", "matrix axis \"s\": value y fills other placeholders than x does"},
		{"plain and named values don't mix", ok + "suites:\n  a: { cmd: 'run {s}', matrix: { s: [x: one, y] } }\n", "value y fills other placeholders than x does"},
		{"what a named value fills is used", ok + "suites:\n  a: { cmd: 'run {v}', matrix: { s: [x: { v: 1, w: 2 }] } }\n", "matrix axis \"s\" is used by no command, fixture or env: {w} is in none"},
		{"a named axis is not a placeholder", ok + "suites:\n  a: { cmd: 'run {s} {v}', matrix: { s: [x: { v: 1 }] } }\n", "unknown placeholder {s}"},
		{"{test-args} comes from a .txt list", ok + "suites:\n  a: { cmd: 'run {test-args}', tests: [t] }\n", "{test-args} is used, but the suite's tests aren't a .txt list"},
		{"{test-args} is only in cmd", ok + "suites:\n  a: { cmd: 'run {test-args}', tests: t.txt, env: { A: '{test-args}' } }\n", "env holds {test-args}, which has a value only in cmd"},
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
		{"{tools} names a tool", ok + "suites:\n  a: { cmd: 'run {tools}' }\n", "{tools}/: no such tool; write {tools}/NAME, NAME one of cthon04, nfstest, pjdfstest, pynfs"},
		{"{tools} names a tool dt has", ok + "suites:\n  a: { cmd: 'run {tools}/pjdfstests/x' }\n", "{tools}/pjdfstests: no such tool"},
		{"an image's own command runs no tests", ok + "suites:\n  a: { image: x, tests: [one, two] }\n", "tests needs a cmd: the image's own command is the same for every test"},
		{"{test} is only in cmd and volumes", ok + "suites:\n  a: { cmd: 'run {test}', tests: [t], dir: '{test}', env: { T: '{test}' }, fixture: { profile: memory, lease: '{test}' } }\n",
			"a: dir holds {test}, which has a value only in cmd and volumes: the rest is filled in before the first test\n" +
				"a: env holds {test}, which has a value only in cmd and volumes: the rest is filled in before the first test\n" +
				"a: fixture holds {test}, which has a value only in cmd and volumes: the rest is filled in before the first test"},
		{"a dfs fixture names its profile", ok + "suites:\n  a: { cmd: true, fixture: { server: dfs, nfs: export } }\n  b: { cmd: true, fixture: { nfs: export } }\n",
			"a: fixture: a dfs fixture names its profile\nb: fixture: a dfs fixture names its profile"},
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

// {test} has a value in cmd and volumes, which dt fills again for each test.
func TestTestPlaceholderInCmdAndVolumes(t *testing.T) {
	y := "version: 1\ndefaults: { format: exit, tier: merge, timeout: { test: 30m, cell: 60m } }\n" +
		"suites:\n  a: { image: x, cmd: 'run {test}', tests: [t], volumes: { '{out}/{test}': /data } }\n"
	if _, err := Parse(t.TempDir(), []byte(y)); err != nil {
		t.Fatal(err)
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
// error, as in a command, and so is {test}, which has no value when
// templates are filled in.
func TestTemplatePlaceholders(t *testing.T) {
	root := t.TempDir()
	for name, body := range map[string]string{
		"ok/a.template":  "{server} {user2} {password} {p} ${KEEP}\n",
		"bad/a.template": "{server}\n",
		"bad/b.template": "{nope}\n",
		"bad/c.template": "{test}\n",
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
	want := "a: template b.template: unknown placeholder {nope}\n" +
		"a: template c.template: {test} has no value before the first test, when templates are filled in"
	if err == nil || err.Error() != want {
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

// A known file is read and checked as the registry loads, and its entries
// kept by its path. Each entry keeps its own rules, and each key of its where
// is a matrix axis of the suite or of an option that takes the file, tier,
// option or os, with a value one of their cells has: a misspelt key or value
// would leave the entry applying nowhere.
func TestKnownFiles(t *testing.T) {
	const y = "version: 1\ndefaults: { format: exit, tier: merge, timeout: { test: 30m, cell: 60m } }\nsuites:\n" +
		"  s:\n    cmd: run {nfs}\n    matrix: { nfs: ['3', '4'] }\n    known: k.yaml\n    options:\n      o: { cmd: 'run {minor}', matrix: { minor: ['1'] } }\n"
	entry := func(where string) string {
		return "- { test: a, status: fail, fix: wontfix, category: proto, reason: r, where: " + where + " }\n"
	}
	for _, c := range []struct {
		name, known, want string
	}{
		{"where takes the suite's axes, an option's, tier, option and os", entry("{ nfs: ['3'], minor: ['1'], tier: [pr], option: [o], os: [macos] }"), ""},
		{"a key that's none of those is refused", entry("{ nsf: ['3'] }"), "k.yaml:1: where nsf: not a matrix axis of the suite, tier, option or os"},
		{"a value no cell has is refused", entry("{ nfs: ['4.1'] }"), `k.yaml:1: where nfs: no cell has the value "4.1"`},
		{"an entry's own rules apply", "- { test: a, status: fail, category: proto, reason: r }\n", "k.yaml:1: fix is required"},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "k.yaml"), []byte(c.known), 0o644); err != nil {
				t.Fatal(err)
			}
			r, err := Parse(root, []byte(y))
			switch {
			case c.want == "" && (err != nil || len(r.Known["k.yaml"]) != 1):
				t.Fatalf("got %v, want the entry loaded", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("got %v, want an error containing %q", err, c.want)
			}
		})
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

// A named matrix value names its cells, and fills the axis's placeholder with
// its value, or with a mapping each of its keys'; a plain value fills the
// axis's own.
func TestNamedMatrixValues(t *testing.T) {
	r, err := Parse(t.TempDir(), []byte(`version: 1
defaults: { format: exit, tier: merge, timeout: { test: 30m, cell: 60m } }
suites:
  a:
    cmd: run {p} {set} {nfs} {opts}
    matrix:
      p: [memory]
      set: [plain: "", v3: vers=3]
      both: [default: { nfs: "4.2", opts: hard }, v3: { nfs: "3", opts: "hard,tcp" }]
`))
	if err != nil {
		t.Fatal(err)
	}
	m := r.Suite("a").Matrix
	if !slices.Equal(m[1].Values, []string{"plain", "v3"}) || !slices.Equal(m[2].Values, []string{"default", "v3"}) {
		t.Fatalf("values %q and %q", m[1].Values, m[2].Values)
	}
	got := m.Fill(map[string]string{"p": "memory", "set": "v3", "both": "default"})
	if want := map[string]string{"p": "memory", "set": "vers=3", "nfs": "4.2", "opts": "hard"}; !maps.Equal(got, want) {
		t.Errorf("filled %v, want %v", got, want)
	}
}

// nfs is a version, or a mapping of a version and options; in the mapping
// the version may be left out, for the options' vers= to give it.
func TestNFSForms(t *testing.T) {
	for y, want := range map[string]NFS{
		`"4.1"`:                        {Version: "4.1"},
		`export`:                       {Version: "export"},
		`{ version: "3", options: x }`: {Version: "3", Options: "x"},
		`{ options: "vers=4.1,noac" }`: {Options: "vers=4.1,noac", FromOptions: true},
		`{ options: "" }`:              {FromOptions: true},
	} {
		var f Fixture
		if err := yaml.Unmarshal([]byte("nfs: "+y), &f); err != nil {
			t.Fatal(err)
		}
		if f.NFS != want || f.NFS.Serves() != true || f.NFS.Mounts() == (want.Version == "export") {
			t.Errorf("%s: %+v, serves %t, mounts %t", y, f.NFS, f.NFS.Serves(), f.NFS.Mounts())
		}
	}
	if (NFS{}).Serves() {
		t.Error("no nfs serves NFS")
	}
}
