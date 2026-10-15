package run

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/test/harness/registry"
)

// A suite whose tests are pjdfstest's, found below {tools}, and which runs
// pynfs by name.
const toolsSuite = `suites:
  pjd:
    cmd: [ "sh {test}", "pynfs-4.1 {test}" ]
    tests: "{tools}/pjdfstest/tests/*/*.t"
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

// fakeTools writes pjdfstest and pynfs as nix builds them, under dir, and
// returns the file their programs record each call in. The pjdfstest binary
// is only in bin, so a test finds it only through the layout of {tools}.
func fakeTools(t *testing.T, dir string) string {
	t.Helper()
	calls := filepath.Join(dir, "calls")
	test := "#!/bin/sh\ndir=`dirname $0`\n. ${dir}/../misc.sh\n"
	files := map[string]string{
		"pjdfstest/bin/pjdfstest":                        "#!/bin/sh\necho \"pjdfstest $1\" >> " + calls + "\n",
		"pjdfstest/share/pjdfstest/tests/misc.sh":        miscSh,
		"pjdfstest/share/pjdfstest/tests/conf":           "",
		"pjdfstest/share/pjdfstest/tests/chmod/00.t":     test,
		"pjdfstest/share/pjdfstest/tests/chmod/01.t":     test,
		"pjdfstest/share/pjdfstest/tests/chmod/notes.md": "",
		"pjdfstest/share/pjdfstest/tests/open/00.t":      test + "exit 1\n",
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
// file, and pynfs is found by name on the PATH the commands get. A suite that
// doesn't name {tools} never builds them.
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

// {tools}/pjdfstest holds a link to pjdfstest's binary beside a copy of its
// tests directory whose directories are real and whose files are links, as
// run-posix.sh's work directory has them; laying it out again replaces it.
func TestToolsLayout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the tools are laid out where they run")
	}
	dtcTools = t.TempDir()
	t.Cleanup(func() { dtcTools = "/opt" })
	fakeTools(t, dtcTools)
	t.Setenv("DT_IN_CONTAINER", "1")
	tools := t.TempDir()
	out := filepath.Join(dtcTools, "pjdfstest")
	for range 2 {
		bins, err := layOutTools(context.Background(), t.TempDir(), tools, &bytes.Buffer{})
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{filepath.Join(out, "bin"), filepath.Join(dtcTools, "pynfs", "bin")}; !slices.Equal(bins, want) {
			t.Errorf("bins %q, want %q", bins, want)
		}
	}
	links := map[string]string{
		"pjdfstest":            filepath.Join(out, "bin", "pjdfstest"),
		"tests/misc.sh":        filepath.Join(out, "share", "pjdfstest", "tests", "misc.sh"),
		"tests/conf":           filepath.Join(out, "share", "pjdfstest", "tests", "conf"),
		"tests/chmod/00.t":     filepath.Join(out, "share", "pjdfstest", "tests", "chmod", "00.t"),
		"tests/open/00.t":      filepath.Join(out, "share", "pjdfstest", "tests", "open", "00.t"),
		"tests/chmod/notes.md": filepath.Join(out, "share", "pjdfstest", "tests", "chmod", "notes.md"),
	}
	for name, target := range links {
		if got, err := os.Readlink(filepath.Join(tools, "pjdfstest", filepath.FromSlash(name))); err != nil || got != target {
			t.Errorf("%s: link to %q, %v; want %q", name, got, err, target)
		}
	}
	for _, d := range []string{"tests", "tests/chmod", "tests/open"} {
		if fi, err := os.Lstat(filepath.Join(tools, "pjdfstest", filepath.FromSlash(d))); err != nil || !fi.IsDir() {
			t.Errorf("%s isn't a directory: %v", d, err)
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

// When the tools can't be had, the cell doesn't run, and says why: nix
// failed, or isn't installed.
func TestToolsMissing(t *testing.T) {
	root, r := repo(t, toolsSuite, nil)
	fakeNix(t, t.TempDir()) // prints directories with no tools in them
	rep, _ := runTarget(t, root, r, "pjd", false)
	if o := rep.Outcomes[0]; o.Result != NotRun || !strings.HasPrefix(o.Note, "the test tools: ") || rep.Status() != 2 {
		t.Errorf("no tools where nix says: %+v", o)
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

// A command's program is found on the PATH the command gets, which can
// differ from dt's own; with the same PATH, exec finds it as before.
func TestProgram(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("programs are found by their execute bits")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "tool"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "data"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	other := []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH")}
	cases := []struct {
		name string
		env  []string
		want string
	}{
		{"tool", other, filepath.Join(bin, "tool")},
		{"data", other, "data"},
		{"./tool", other, "./tool"},
		{"tool", []string{"PATH=" + os.Getenv("PATH")}, "tool"},
		{"tool", []string{"PATH=relative"}, "tool"},
	}
	for _, c := range cases {
		if got := program(c.name, c.env); got != c.want {
			t.Errorf("%s with %q: %q, want %q", c.name, c.env, got, c.want)
		}
	}
	if got := envPath(prependPath([]string{"PATH=/a", "PATH=/b"}, []string{"/x", "/y"})); got != "/x"+string(os.PathListSeparator)+"/y"+string(os.PathListSeparator)+"/b" {
		t.Errorf("prepended PATH %q", got)
	}
}
