package run

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/test/harness/registry"
)

// at runs group g with up to n cells at once, and returns the report and what
// the run printed.
func at(t *testing.T, root string, r *registry.Registry, n int) (Report, string) {
	t.Helper()
	cells, err := Select(root, r, "g", "", "pr", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	rep, err := Run(context.Background(), cells, Options{Root: root, Results: filepath.Join(root, "results"), Out: &out, Parallel: n})
	if err != nil {
		t.Fatal(err)
	}
	return rep, out.String()
}

// count.sh marks its cell as running for a while, and fails when more than
// $2 cells ran at once as it started.
const count = `mkdir -p marks && : > "marks/$1"
n=$(ls marks | wc -l)
sleep 0.3
rm "marks/$1"
[ "$n" -le "$2" ]
`

// With --parallel 2, two cells run at once: each waits for the other to
// start. Their results come in the cells' order, and each line they print is
// led by its cell's name.
func TestParallelCellsRunAtOnce(t *testing.T) {
	root, r := repo(t, `groups: { g: [a, b] }
suites:
  a: { cmd: "sh t/meet.sh a", parallel: true }
  b: { cmd: "sh t/meet.sh b", parallel: true }
`, map[string]string{"t/meet.sh": `mkdir -p marks && : > "marks/$1" && echo "hello from $1"
i=0
while [ "$(ls marks | wc -l)" -lt 2 ]; do
  i=$((i + 1)); [ "$i" -gt 60 ] && exit 1
  sleep 0.05
done
`})
	rep, out := at(t, root, r, 2)
	if len(rep.Outcomes) != 2 || rep.Outcomes[0].Name != "a" || rep.Outcomes[1].Name != "b" || rep.Status() != 0 {
		t.Fatalf("outcomes %+v\n%s", rep.Outcomes, out)
	}
	for _, want := range []string{"\n[a] hello from a\n", "\n[b] hello from b\n"} {
		if !strings.Contains("\n"+out, want) {
			t.Errorf("no line %q in:\n%s", strings.TrimSpace(want), out)
		}
	}
}

// --parallel N runs no more than N cells at once.
func TestParallelHoldsToN(t *testing.T) {
	root, r := repo(t, `groups: { g: [a, b, c] }
suites:
  a: { cmd: "sh t/count.sh a 2", parallel: true }
  b: { cmd: "sh t/count.sh b 2", parallel: true }
  c: { cmd: "sh t/count.sh c 2", parallel: true }
`, map[string]string{"t/count.sh": count})
	if rep, out := at(t, root, r, 2); rep.Status() != 0 {
		t.Errorf("outcomes %+v\n%s", rep.Outcomes, out)
	}
}

// Cells that conflict run one at a time, whatever --parallel allows: here a
// suite that doesn't say parallel.
func TestConflictingCellsRunApart(t *testing.T) {
	root, r := repo(t, `groups: { g: [a, b] }
suites:
  a: { cmd: "sh t/count.sh a 1" }
  b: { cmd: "sh t/count.sh b 1", parallel: true }
`, map[string]string{"t/count.sh": count})
	if rep, out := at(t, root, r, 2); rep.Status() != 0 {
		t.Errorf("outcomes %+v\n%s", rep.Outcomes, out)
	}
}

// Two cells conflict when either's suite doesn't say parallel, either needs
// free disk, both need the host's rpcbind, as knfsd does too, or both need
// the same port; their needs hold their fixtures'.
func TestConflict(t *testing.T) {
	cell := func(parallel bool, needs ...string) Cell {
		c := Cell{Unit: &registry.Suite{Parallel: parallel}}
		c.Needs = needs
		return c
	}
	for _, c := range []struct {
		name string
		a, b Cell
		want bool
	}{
		{"a suite that doesn't say parallel", cell(false), cell(true), true},
		{"both say parallel", cell(true, "linux", "root"), cell(true, "linux", "nfs-client"), false},
		{"free disk", cell(true, "disk:110"), cell(true), true},
		{"both on the host's rpcbind", cell(true, "rpcbind"), cell(true, "linux", "rpcbind"), true},
		{"knfsd and the host's rpcbind", cell(true, "root", "knfsd", "port:2049"), cell(true, "rpcbind"), true},
		{"the host's rpcbind beside none", cell(true, "rpcbind"), cell(true, "root"), false},
		{"the same port", cell(true, "port:445"), cell(true, "root", "port:445"), true},
		{"other ports", cell(true, "port:445"), cell(true, "port:2049"), false},
	} {
		if got := conflict(c.a, c.b); got != c.want {
			t.Errorf("%s: conflict %v, want %v", c.name, got, c.want)
		}
	}
}

// With --parallel, the run's {bin} is built once, before any cell starts,
// not by each fixture cell at once into the same directory.
func TestParallelBuildsBinOnce(t *testing.T) {
	fakeFixture(t, func(context.Context) (fixtureServer, error) { return &fakeServer{}, nil })
	root, r := repo(t, `groups: { g: [a, b] }
suites:
  a: { cmd: "true", parallel: true, fixture: { profile: memory, nfs: export } }
  b: { cmd: "true", parallel: true, fixture: { profile: memory, nfs: export } }
`, map[string]string{"fake/go": `#!/bin/sh
mkdir "$GO_LOCK" 2>/dev/null || echo overlap >>"$GO_LOG"
echo build >>"$GO_LOG"
while [ $# -gt 0 ]; do [ "$1" = -o ] && out=$2; shift; done
sleep 0.3
mkdir -p "$out" && : >"$out/dfs" && : >"$out/dfsctl"
rmdir "$GO_LOCK" 2>/dev/null
exit 0
`})
	t.Setenv("PATH", filepath.Join(root, "fake")+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GO_LOCK", filepath.Join(root, "go.lock"))
	t.Setenv("GO_LOG", filepath.Join(root, "go.log"))
	rep, out := at(t, root, r, 2)
	log, _ := os.ReadFile(filepath.Join(root, "go.log"))
	if rep.Status() != 0 || string(log) != "build\n" {
		t.Errorf("go build ran %q, outcomes %+v\n%s", log, rep.Outcomes, out)
	}
}
