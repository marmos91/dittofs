package run

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/test/harness/registry"
)

// Suites of the test tools: pjd runs pjdfstest's tests, found below {tools},
// and pynfs by its path there; only runs pjdfstest's tests alone, in two
// cells.
const toolsSuite = `suites:
  pjd:
    cmd: [ "sh {test}", "{tools}/pynfs/bin/pynfs-4.1 {test}" ]
    tests: "{tools}/pjdfstest/tests/*/*.t"
  only:
    cmd: "sh {test} {p}"
    tests: "{tools}/pjdfstest/tests/*/*.t"
    matrix: { p: [a, b] }
  plain: { cmd: "true" }
`

// The walk pjdfstest's misc.sh does to find its binary, from the directory of
// the test that sources it up to /.
const miscSh = `maindir=${dir:-$(dirname "$0")}
while [ "$maindir" != / ]; do
	if [ -f "$maindir/pjdfstest" -a -x "$maindir/pjdfstest" ]; then
		break
	fi
	maindir=$(cd $maindir/../; pwd)
done
"$maindir/pjdfstest" "$maindir"
`

// fakeTools writes pjdfstest and pynfs under dir as nix builds them, the
// pjdfstest binary in bin and beside its tests in share/pjdfstest, and returns
// the file their programs record each call in.
func fakeTools(t *testing.T, dir string) string {
	t.Helper()
	calls := filepath.Join(dir, "calls")
	test := "#!/bin/sh\ndir=`dirname $0`\n. ${dir}/../misc.sh\n"
	binary := "#!/bin/sh\necho \"pjdfstest $1\" >> " + calls + "\n"
	files := map[string]string{
		"pjdfstest/bin/pjdfstest":                        binary,
		"pjdfstest/share/pjdfstest/pjdfstest":            binary,
		"pjdfstest/share/pjdfstest/tests/misc.sh":        miscSh,
		"pjdfstest/share/pjdfstest/tests/conf":           "",
		"pjdfstest/share/pjdfstest/tests/chmod/00.t":     test,
		"pjdfstest/share/pjdfstest/tests/chmod/01.t":     test,
		"pjdfstest/share/pjdfstest/tests/chmod/notes.md": "",
		"pjdfstest/share/pjdfstest/tests/open/00.t":      test + "exit 1\n",
		"pynfs/bin/pynfs-4.0":                            "#!/bin/sh\necho \"pynfs-4.0 $1\" >> " + calls + "\n",
		"pynfs/bin/pynfs-4.1":                            "#!/bin/sh\necho \"pynfs-4.1 $1\" >> " + calls + "\n",
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return calls
}

// fakeNix puts a nix on PATH that prints the fake tools' directories for
// their packages, and returns the file it records each call in.
func fakeNix(t *testing.T, outs string) string {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(bin, "nix.log")
	script := "#!/bin/sh\necho \"$(pwd -P) $*\" >> " + log + "\ncase \"$2\" in\n" +
		".#pjdfstest) echo " + filepath.Join(outs, "pjdfstest") + " ;;\n" +
		".#pynfs) echo " + filepath.Join(outs, "pynfs") + " ;;\n*) exit 1 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "nix"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DT_IN_CONTAINER", "")
	return log
}

func lines(t *testing.T, file string) []string {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// {tools} is laid out from what nix builds of the flake's pjdfstest and
// pynfs, in the repository: pjdfstest's tests find its binary through it,
// each test is named by its path below the tests directory and runs by its
// file, and pynfs runs by its path there. A suite that doesn't name {tools}
// never builds them.
func TestToolsFromNix(t *testing.T) {
	root, r := repo(t, toolsSuite, nil)
	outs := t.TempDir()
	calls := fakeTools(t, outs)
	nixLog := fakeNix(t, outs)

	if rep, _ := runTarget(t, root, r, "plain", false); rep.Outcomes[0].Result != Passed {
		t.Fatalf("plain: %+v", rep.Outcomes)
	}
	if _, err := os.Stat(nixLog); err == nil {
		t.Error("a suite without {tools} built them")
	}

	rep, _ := runTarget(t, root, r, "pjd", false)
	tools := filepath.Join(rep.Dir, "tools")
	o := rep.Outcomes[0]
	open := filepath.Join(tools, "pjdfstest", "tests", "open", "00.t")
	if o.Result != Failed || o.Note != "open/00.t: `sh "+open+"` exited 1" {
		t.Errorf("outcome %+v", o)
	}
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	wantNix := []string{
		real + " build .#pjdfstest --no-link --print-out-paths",
		real + " build .#pynfs --no-link --print-out-paths",
	}
	if got := lines(t, nixLog); !slices.Equal(got, wantNix) {
		t.Errorf("nix calls %q, want %q", got, wantNix)
	}
	var want []string
	for _, test := range []string{"chmod/00.t", "chmod/01.t", "open/00.t"} {
		want = append(want, "pjdfstest "+filepath.Join(tools, "pjdfstest"),
			"pynfs-4.1 "+filepath.Join(tools, "pjdfstest", "tests", filepath.FromSlash(test)))
	}
	if got := lines(t, calls); !slices.Equal(got, want) {
		t.Errorf("calls\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// Inside dtc the tools are the image's copies, and nix isn't called.
func TestToolsInDtc(t *testing.T) {
	root, r := repo(t, toolsSuite, nil)
	dtcTools = t.TempDir()
	t.Cleanup(func() { dtcTools = "/opt" })
	calls := fakeTools(t, dtcTools)
	nixLog := fakeNix(t, t.TempDir())
	t.Setenv("DT_IN_CONTAINER", "1")

	rep, _ := runTarget(t, root, r, "pjd", false)
	if o := rep.Outcomes[0]; o.Result != Failed || !strings.HasPrefix(o.Note, "open/00.t: ") {
		t.Errorf("outcome %+v", o)
	}
	if got := lines(t, calls); len(got) != 6 {
		t.Errorf("calls %q", got)
	}
	if _, err := os.Stat(nixLog); err == nil {
		t.Error("nix was called inside dtc")
	}
}

// A suite gets only the tools it names, each laid out once per run: a
// pjdfstest suite runs without pynfs, and its second cell keeps the first's
// link.
func TestToolsOnlyNamed(t *testing.T) {
	root, r := repo(t, toolsSuite, nil)
	outs := t.TempDir()
	fakeTools(t, outs)
	if err := os.RemoveAll(filepath.Join(outs, "pynfs")); err != nil {
		t.Fatal(err)
	}
	nixLog := fakeNix(t, outs)
	rep, _ := runTarget(t, root, r, "only", false)
	if len(rep.Outcomes) != 2 {
		t.Fatalf("outcomes %+v", rep.Outcomes)
	}
	for _, o := range rep.Outcomes {
		if o.Result != Failed || !strings.HasPrefix(o.Note, "open/00.t: ") {
			t.Errorf("outcome %+v", o)
		}
	}
	if got := lines(t, nixLog); len(got) != 1 || !strings.HasSuffix(got[0], " build .#pjdfstest --no-link --print-out-paths") {
		t.Errorf("nix calls %q", got)
	}
	if _, err := os.Lstat(filepath.Join(rep.Dir, "tools", "pynfs")); err == nil {
		t.Error("pynfs was laid out for a suite that doesn't name it")
	}
}

// {tools}/pjdfstest links to pjdfstest's share/pjdfstest, its binary beside
// its tests, and {tools}/pynfs to pynfs. A link already there is kept and
// nothing is built for it, so laying out again never removes what another
// cell uses.
func TestToolsLayout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the tools are laid out where they run")
	}
	dtcTools = t.TempDir()
	t.Cleanup(func() { dtcTools = "/opt" })
	fakeTools(t, dtcTools)
	t.Setenv("DT_IN_CONTAINER", "1")
	tools := t.TempDir()
	links := map[string]string{
		"pjdfstest": filepath.Join(dtcTools, "pjdfstest", "share", "pjdfstest"),
		"pynfs":     filepath.Join(dtcTools, "pynfs"),
	}
	for range 2 {
		if err := layOutTools(context.Background(), t.TempDir(), tools, []string{"pjdfstest", "pynfs"}, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		for name, target := range links {
			if got, err := os.Readlink(filepath.Join(tools, name)); err != nil || got != target {
				t.Errorf("%s: link to %q, %v; want %q", name, got, err, target)
			}
		}
		dtcTools = t.TempDir() // no tools here: laying out again builds nothing
	}
	for _, p := range []string{"pjdfstest/pjdfstest", "pjdfstest/tests/misc.sh", "pjdfstest/tests/chmod/00.t", "pynfs/bin/pynfs-4.1"} {
		if _, err := os.Stat(filepath.Join(tools, filepath.FromSlash(p))); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
}

// The registry's pjdfstest and pynfs cells each name the one tool they use,
// and pynfs's command runs pynfs-4.{minor} from it.
func TestRegistryNamesItsTools(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the tools are laid out where they run")
	}
	dtcTools = t.TempDir()
	t.Cleanup(func() { dtcTools = "/opt" })
	fakeTools(t, dtcTools)
	t.Setenv("DT_IN_CONTAINER", "1")
	tools := t.TempDir()
	if err := layOutTools(context.Background(), t.TempDir(), tools, []string{"pynfs"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	r, err := registry.Load("../../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, suite := range []string{"pjdfstest", "pynfs"} {
		cells, err := Select("../../..", r, suite, "", "pr", nil, nil, nil)
		if err != nil || len(cells) == 0 {
			t.Fatalf("%s: %+v, %v", suite, cells, err)
		}
		for _, c := range cells {
			if got := c.Unit.Tools(); !slices.Equal(got, []string{suite}) {
				t.Errorf("%s names the tools %q", c.Name, got)
			}
			if suite != "pynfs" {
				continue
			}
			vals := map[string]string{"tools": tools, "server": "10.0.0.1", "out": "/out", "minor": c.Matrix["minor"]}
			argv, err := fill(c.Unit.Cmd[0], vals, nil)
			if err != nil {
				t.Fatal(err)
			}
			if want := filepath.Join(tools, "pynfs", "bin", "pynfs-4."+c.Matrix["minor"]); argv[0] != want {
				t.Errorf("%s runs %q, want %q", c.Name, argv[0], want)
			}
			if fi, err := os.Stat(argv[0]); err != nil || fi.Mode()&0o111 == 0 {
				t.Errorf("%s: %s isn't a program: %v", c.Name, argv[0], err)
			}
		}
	}
}

// Tests below {tools} are chosen by name or file name when the cell runs, so
// selecting them builds nothing; a name that picks none leaves the cell not
// run, with nothing run. dt run pjdfstest chmod/00.t selects that test in
// every cell of the real registry.
func TestToolsSelect(t *testing.T) {
	root, r := repo(t, toolsSuite, nil)
	outs := t.TempDir()
	calls := fakeTools(t, outs)
	nixLog := fakeNix(t, outs)
	run := func(names ...string) (Report, string) {
		t.Helper()
		cells, err := Select(root, r, "pjd", "", "pr", nil, nil, names)
		if err != nil || len(cells) != 1 || !slices.Equal(cells[0].Tests, names) {
			t.Fatalf("%q: cells %+v, %v", names, cells, err)
		}
		if _, err := os.Stat(nixLog); err == nil {
			t.Fatal("selecting tests built the tools")
		}
		var out bytes.Buffer
		rep, err := Run(context.Background(), cells, Options{Root: root, Results: filepath.Join(root, "results"), Out: &out})
		if err != nil {
			t.Fatal(err)
		}
		_ = os.Remove(nixLog)
		return rep, out.String()
	}

	rep, out := run("chmod/00.t")
	if rep.Outcomes[0].Result != Passed || strings.Contains(out, "chmod/01.t") || len(lines(t, calls)) != 2 {
		t.Errorf("chmod/00.t: %+v\n%s", rep.Outcomes[0], out)
	}
	_ = os.Remove(calls)
	rep, _ = run("00.t")
	if o := rep.Outcomes[0]; o.Result != Failed || !strings.HasPrefix(o.Note, "open/00.t: ") || len(lines(t, calls)) != 4 {
		t.Errorf("00.t: %+v", o)
	}
	_ = os.Remove(calls)
	rep, _ = run("chmod/00.t", "chmod/99.t")
	if o := rep.Outcomes[0]; o.Result != NotRun || o.Note != `no test "chmod/99.t" in {tools}/pjdfstest/tests/*/*.t` || rep.Status() != 2 {
		t.Errorf("chmod/99.t: %+v", o)
	}
	if _, err := os.Stat(calls); err == nil {
		t.Error("a test ran though a name picked none")
	}

	real, err := registry.Load("../../..")
	if err != nil {
		t.Fatal(err)
	}
	cells, err := Select("../../..", real, "pjdfstest", "", "pr", nil, nil, []string{"chmod/00.t"})
	if err != nil || len(cells) != 3 {
		t.Fatalf("pjdfstest chmod/00.t: %+v, %v", cells, err)
	}
	for _, c := range cells {
		if !slices.Equal(c.Tests, []string{"chmod/00.t"}) {
			t.Errorf("%s: tests %q", c.Name, c.Tests)
		}
	}
}

// When the tools can't be had, the cell doesn't run, and says why: what nix
// built lacks a tool's programs, or nix failed, or isn't installed.
func TestToolsMissing(t *testing.T) {
	root, r := repo(t, toolsSuite, nil)
	outs := t.TempDir()
	fakeTools(t, outs)
	if err := os.RemoveAll(filepath.Join(outs, "pynfs", "bin")); err != nil {
		t.Fatal(err)
	}
	fakeNix(t, outs)
	rep, _ := runTarget(t, root, r, "pjd", false)
	if o := rep.Outcomes[0]; o.Result != NotRun || !strings.HasPrefix(o.Note, "the test tools: ") || !strings.Contains(o.Note, filepath.Join("pynfs", "bin")) || rep.Status() != 2 {
		t.Errorf("no pynfs programs: %+v", o)
	}

	nix := t.TempDir()
	if err := os.WriteFile(filepath.Join(nix, "nix"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", nix)
	rep, _ = runTarget(t, root, r, "pjd", false)
	if o := rep.Outcomes[0]; o.Result != NotRun || o.Note != "the test tools: `nix build .#pjdfstest --no-link --print-out-paths` exited 1" {
		t.Errorf("nix failed: %+v", o)
	}

	t.Setenv("PATH", t.TempDir())
	rep, _ = runTarget(t, root, r, "pjd", false)
	if o := rep.Outcomes[0]; o.Result != NotRun || !strings.Contains(o.Note, "nix isn't installed") {
		t.Errorf("no nix: %+v", o)
	}
}

// The dtc image checks each tool out at the revision flake.lock pins for its
// source, so its copies under /opt, which dt takes inside dtc, are what nix
// builds elsewhere. A revision changed on one side only fails.
func TestDtcToolsMatchFlakeLock(t *testing.T) {
	lock, err := os.ReadFile("../../../flake.lock")
	if err != nil {
		t.Fatal(err)
	}
	dockerfile, err := os.ReadFile("../docker/dev.Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range pinDrift(lock, dockerfile) {
		t.Error(d)
	}
	// A checkout may end its lines with \r\n.
	if d := pinDrift(lock, []byte(strings.ReplaceAll(string(dockerfile), "\n", "\r\n"))); len(d) != 0 {
		t.Errorf("CRLF line endings give %q", d)
	}
	changed := strings.Replace(string(dockerfile), "PYNFS_REV=", "PYNFS_REV=0", 1)
	if d := pinDrift(lock, []byte(changed)); len(d) != 1 || !strings.Contains(d[0], "PYNFS_REV=0") {
		t.Errorf("a changed revision gives %q", d)
	}
}

// pinDrift compares, for each tool dt has, the revision flake.lock pins for
// its NAME-src input with the NAME_REV the Dockerfile checks out, and says
// how each differs.
func pinDrift(lock, dockerfile []byte) []string {
	var l struct {
		Nodes map[string]struct{ Locked struct{ Rev string } }
	}
	if err := json.Unmarshal(lock, &l); err != nil {
		return []string{"flake.lock: " + err.Error()}
	}
	var out []string
	for _, name := range slices.Sorted(maps.Keys(registry.ToolDirs)) {
		input, arg := name+"-src", strings.ToUpper(name)+"_REV"
		pinned := l.Nodes[input].Locked.Rev
		m := regexp.MustCompile(`(?m)^ARG ` + arg + `=(\S+)\r?$`).FindSubmatch(dockerfile)
		switch {
		case pinned == "":
			out = append(out, "flake.lock pins no "+input)
		case m == nil:
			out = append(out, "the Dockerfile sets no "+arg)
		case string(m[1]) != pinned:
			out = append(out, fmt.Sprintf("the Dockerfile has %s=%s, but flake.lock pins %s at %s", arg, m[1], input, pinned))
		}
	}
	return out
}
