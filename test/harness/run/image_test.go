package run

import (
	"bytes"
	"context"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/harness/report"
)

var update = flag.Bool("update", false, "rewrite the golden files from what the tests get")

// fakeDocker puts a docker on PATH that records each call's arguments, one
// per line and a blank line after the call, and returns the file it records
// them in. docker image inspect finds the image unless FAKE_IMAGE is set;
// docker pull stalls when it's "stall", fails when it's "fail", and else
// passes. docker run touches the log's name with .run added once it has
// started, prints the value of each variable -e names, and runs the command
// after the image as the container would, or says it ran the image's own.
// docker rm does nothing more.
func fakeDocker(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(bin, "docker.log")
	script := `#!/bin/sh
{ for a in "$@"; do printf '%s\n' "$a"; done; echo; } >> ` + log + `
case "$1" in
image) [ -z "$FAKE_IMAGE" ]; exit ;;
pull) case "$FAKE_IMAGE" in stall) exec sleep 30 ;; fail) exit 1 ;; esac; exit 0 ;;
run) ;;
*) exit 0 ;;
esac
: > ` + log + `.run
shift
while :; do
	case "$1" in
	--rm) shift ;;
	--name|--network|-v) shift 2 ;;
	-e) echo "env $2=$(printenv "$2")"; shift 2 ;;
	*) break ;;
	esac
done
shift
[ $# -gt 0 ] || { echo "the image's own command"; exit 0; }
exec "$@"
`
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, k := range []string{"DT_IN_CONTAINER", "DT_HOSTNET", "DT_CONTAINER_NAME", "FAKE_IMAGE"} {
		t.Setenv(k, "")
	}
	return log
}

// calls returns the docker calls log records, each as its arguments.
func calls(t *testing.T, log string) [][]string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	var out [][]string
	for _, call := range strings.Split(strings.TrimSuffix(string(data), "\n\n"), "\n\n") {
		out = append(out, strings.Split(call, "\n"))
	}
	return out
}

// The cell pulls a missing image once, before its first test. Then a
// command in an image runs as docker run --rm, with a name of its own, on
// the host's network, with -e naming each of the suite's variables, whose
// values reach the container through docker's environment, and -v for each
// volume, its host path from the repository root unless it's absolute. An
// sh command runs through sh -c in the image, and an image without a cmd runs
// its own command.
func TestImageDockerArgs(t *testing.T) {
	root, r := repo(t, `suites:
  img:
    image: example.com/tool:1
    cmd: [ "echo ran {p} {out}", { sh: "echo $Usage {p}" } ]
    env: { Usage: RunTestCases, Filter: "TestCategory=BVT" }
    matrix: { p: [a] }
    volumes: { "{out}/config": /data/fileserver, "rel/dir": /data/rel }
  own: { image: example.com/own:2 }
`, nil)
	log := fakeDocker(t)
	t.Setenv("FAKE_IMAGE", "missing")
	rep, out := runTarget(t, root, r, "img", false)
	if o := rep.Outcomes[0]; o.Result != Passed {
		t.Fatalf("img: %+v\n%s", o, out)
	}
	if rep, out := runTarget(t, root, r, "own", false); rep.Outcomes[0].Result != Passed || !strings.Contains(out, "the image's own command") {
		t.Fatalf("own: %+v\n%s", rep.Outcomes[0], out)
	}
	cellOut := filepath.Join(rep.Dir, "img-a")
	for _, want := range []string{"env Usage=RunTestCases", "env Filter=TestCategory=BVT", "ran a " + cellOut, "RunTestCases a"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	got := []byte(regexp.MustCompile(`(?m)^dt-(img-a|own)-[0-9a-f]{8}$`).ReplaceAllString(
		strings.NewReplacer(cellOut, "{out}", root, "ROOT").Replace(readFile(t, log)), "dt-$1-NAME"))
	golden(t, "docker-run.golden", got)
}

// The network is the one the fixture's server is on: the host's, or, for dt
// in a dtc container on a bridge network, that container's.
func TestImageNetwork(t *testing.T) {
	for _, c := range []struct{ in, hostnet, want string }{
		{"", "", "host"},
		{"1", "1", "host"},
		{"1", "", "container:dtc-1"},
	} {
		t.Setenv("DT_IN_CONTAINER", c.in)
		t.Setenv("DT_HOSTNET", c.hostnet)
		t.Setenv("DT_CONTAINER_NAME", "dtc-1")
		if got := network(); got != c.want {
			t.Errorf("in %q, hostnet %q: %s, want %s", c.in, c.hostnet, got, c.want)
		}
	}
}

// A test stopped at its limit, at the cell's or by an interrupt has its
// container removed by name: killing docker's client leaves the container
// running.
func TestKillRemovesContainer(t *testing.T) {
	root, r := repo(t, `suites:
  slow: { image: x, cmd: "sleep 30", timeout: { test: 1s } }
  cell: { image: x, cmd: "sleep 30", timeout: { test: 10s, cell: 1m } }
  stop: { image: x, cmd: "sleep 30" }
`, nil)
	minute = 500 * time.Millisecond
	t.Cleanup(func() { minute = time.Minute })
	for _, suite := range []string{"slow", "cell", "stop"} {
		t.Run(suite, func(t *testing.T) {
			log := fakeDocker(t)
			cells, err := Select(root, r, suite, "", "pr", nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if suite == "stop" {
				go func() { // the interrupt, once docker run has started
					for ctx.Err() == nil {
						if _, err := os.Stat(log + ".run"); err == nil {
							cancel()
						}
						time.Sleep(10 * time.Millisecond)
					}
				}()
			}
			if _, err := Run(ctx, cells, Options{Root: root, Results: filepath.Join(root, "results"), Out: &bytes.Buffer{}}); err != nil {
				t.Fatal(err)
			}
			got := calls(t, log)
			run, rm := got[len(got)-2], got[len(got)-1]
			if run[0] != "run" || run[2] != "--name" {
				t.Fatalf("calls %q", got)
			}
			if want := []string{"rm", "-f", run[3]}; strings.Join(rm, " ") != strings.Join(want, " ") {
				t.Errorf("after the kill: %q, want %q", rm, want)
			}
		})
	}
}

// dt removes no container that exited, which --rm removes, and none whose
// docker client never started, as when the test's limit passed first.
func TestOnlyAKilledContainerIsRemoved(t *testing.T) {
	root, r := repo(t, `suites:
  exited: { image: x, cmd: "false" }
  unstarted: { image: x, cmd: "true", timeout: { test: 1ns } }
`, nil)
	for suite, runs := range map[string]int{"exited": 1, "unstarted": 0} {
		log := fakeDocker(t)
		if rep, _ := runTarget(t, root, r, suite, false); rep.Outcomes[0].Result != Failed {
			t.Fatalf("%s: %+v", suite, rep.Outcomes[0])
		}
		got := map[string]int{}
		for _, call := range calls(t, log) {
			got[call[0]]++
		}
		if got["run"] != runs || got["rm"] != 0 {
			t.Errorf("%s: calls %q", suite, calls(t, log))
		}
	}
}

// The image is pulled before the first test, under a limit of its own, and
// only when docker doesn't have it. A pull that fails or stalls leaves the
// cell not run, and runs no test.
func TestImagePull(t *testing.T) {
	root, r := repo(t, `suites:
  s: { image: x, cmd: "true", timeout: { test: 10s } }
`, nil)
	pullLimit = 300 * time.Millisecond
	t.Cleanup(func() { pullLimit = 5 * time.Minute })
	for _, c := range []struct{ image, result, note, calls string }{
		{"", Passed, "", "image run"},
		{"fail", NotRun, "the image: `docker pull x` exited 1", "image pull"},
		{"stall", NotRun, "the image: `docker pull x` took over 300ms", "image pull"},
	} {
		log := fakeDocker(t)
		t.Setenv("FAKE_IMAGE", c.image)
		rep, out := runTarget(t, root, r, "s", false)
		if o := rep.Outcomes[0]; o.Result != c.result || o.Note != c.note {
			t.Errorf("%q: %+v\n%s", c.image, o, out)
		}
		var got []string
		for _, call := range calls(t, log) {
			got = append(got, call[0])
		}
		if strings.Join(got, " ") != c.calls {
			t.Errorf("%q: calls %q, want %s", c.image, got, c.calls)
		}
	}
}

// docker run's own exit statuses, 125 to 127, mean the container didn't
// start: the test didn't run, and nor did the cell, unless another test
// failed. In a format a reader reads, that holds whatever the reader finds.
func TestContainerNotStartedIsNotRun(t *testing.T) {
	readers["tap"] = func(r report.Ran) ([]report.Test, error) {
		return []report.Test{{Name: r.Test, Status: report.Passed}}, nil
	}
	t.Cleanup(func() { delete(readers, "tap") })
	root, r := repo(t, `suites:
  exit: { image: x, cmd: "sh {test}", tests: "e/*.sh" }
  read: { image: x, cmd: "sh {test}", tests: "e/*.sh", format: tap }
  mixed: { image: x, cmd: "sh {test}", tests: "m/*.sh" }
  host: { cmd: "sh {test}", tests: "h/*.sh" }
`, map[string]string{
		"e/a.sh": "exit 125\n", "e/b.sh": "exit 126\n", "e/c.sh": "exit 127\n", "e/d.sh": "exit 0\n",
		"m/a.sh": "exit 125\n", "m/b.sh": "exit 1\n",
		"h/a.sh": "exit 125\n",
	})
	fakeDocker(t)
	for _, suite := range []string{"exit", "read"} {
		rep, _ := runTarget(t, root, r, suite, false)
		o := rep.Outcomes[0]
		for _, code := range []string{"125", "126", "127"} {
			if !strings.Contains(o.Note, "exited "+code+": its container didn't start") {
				t.Errorf("%s: note %q lacks %s", suite, o.Note, code)
			}
		}
		if o.Result != NotRun || rep.Status() != 2 {
			t.Errorf("%s: %+v, status %d", suite, o, rep.Status())
		}
	}
	if rep, _ := runTarget(t, root, r, "mixed", false); rep.Outcomes[0].Result != Failed || rep.Status() != 1 {
		t.Errorf("mixed: %+v, status %d", rep.Outcomes[0], rep.Status())
	}
	// A command on the host that exits 125 is its own failure.
	if rep, _ := runTarget(t, root, r, "host", false); rep.Outcomes[0].Result != Failed {
		t.Errorf("host: %+v", rep.Outcomes[0])
	}
}

// Templates are filled in with the cell's placeholders into {out}/config,
// each under its name without .template, before the first test, leaving a
// ${VAR} alone. One dt can't fill leaves the cell not run, and runs nothing.
func TestTemplates(t *testing.T) {
	root, r := repo(t, `suites:
  ok: { image: x, cmd: "cat {out}/config/a.conf", templates: "t/*.template", matrix: { p: [v] }, env: { P: "{p}" } }
  unfilled: { image: x, templates: "u/*.template" }
`, map[string]string{
		"t/a.conf.template": "p={p} out={out} keep=${KEEP}\n",
		"u/b.template":      "{server}\n",
	})
	log := fakeDocker(t)
	rep, out := runTarget(t, root, r, "ok", false)
	if o := rep.Outcomes[0]; o.Result != Passed {
		t.Fatalf("ok: %+v\n%s", o, out)
	}
	cellOut := filepath.Join(rep.Dir, "ok-v")
	if got, want := readFile(t, filepath.Join(cellOut, "config", "a.conf")), "p=v out="+cellOut+" keep=${KEEP}\n"; got != want {
		t.Errorf("a.conf: %q, want %q", got, want)
	}
	if !strings.Contains(out, "keep=${KEEP}") {
		t.Errorf("the command didn't find the file:\n%s", out)
	}
	_ = os.Remove(log)
	rep, _ = runTarget(t, root, r, "unfilled", false)
	if o := rep.Outcomes[0]; o.Result != NotRun || !strings.Contains(o.Note, "the templates: b.template: dt doesn't provide {server} yet") {
		t.Errorf("unfilled: %+v", o)
	}
	if _, err := os.Stat(log); err == nil {
		t.Error("a cell whose templates dt can't fill ran docker")
	}
}

func readFile(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// golden compares got with testdata/name, or writes it there with -update.
// A checkout may have turned the file's line ends into CRLF.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n"))) {
		t.Errorf("%s differs; got\n%s", path, got)
	}
}
