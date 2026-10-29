package run

import (
	"bytes"
	"context"
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
  a: { cmd: "sh t/meet.sh a" }
  b: { cmd: "sh t/meet.sh b" }
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
  a: { cmd: "sh t/count.sh a 2" }
  b: { cmd: "sh t/count.sh b 2" }
  c: { cmd: "sh t/count.sh c 2" }
`, map[string]string{"t/count.sh": count})
	if rep, out := at(t, root, r, 2); rep.Status() != 0 {
		t.Errorf("outcomes %+v\n%s", rep.Outcomes, out)
	}
}

// Cells that conflict run one at a time, whatever --parallel allows.
func TestConflictingCellsRunApart(t *testing.T) {
	root, r := repo(t, `groups: { g: [a, b] }
suites:
  a: { cmd: "sh t/count.sh a 1", exclusive: true }
  b: { cmd: "sh t/count.sh b 1" }
`, map[string]string{"t/count.sh": count})
	if rep, out := at(t, root, r, 2); rep.Status() != 0 {
		t.Errorf("outcomes %+v\n%s", rep.Outcomes, out)
	}
}

// Two cells conflict when either is exclusive, when their fixtures set the
// same port, or when both use the host's rpcbind, as a knfsd server does.
func TestConflict(t *testing.T) {
	cell := func(u registry.Suite) Cell { return Cell{Unit: &u} }
	smb := func(port int) registry.Suite {
		return registry.Suite{Fixture: &registry.Fixture{SMB: registry.SMB{Port: port}}}
	}
	rpc := registry.Suite{Fixture: &registry.Fixture{RPCBind: "host"}}
	knfsd := registry.Suite{Fixture: &registry.Fixture{Server: "knfsd"}}
	plain := registry.Suite{Fixture: &registry.Fixture{}}
	for _, c := range []struct {
		name string
		a, b registry.Suite
		want bool
	}{
		{"exclusive", registry.Suite{Exclusive: true}, plain, true},
		{"same port", smb(445), smb(445), true},
		{"port and free ports", smb(445), smb(0), false},
		{"both on the host's rpcbind", rpc, rpc, true},
		{"knfsd and the host's rpcbind", knfsd, rpc, true},
		{"the host's rpcbind and its own", rpc, plain, false},
		{"no fixture", registry.Suite{}, registry.Suite{}, false},
	} {
		if got := conflict(cell(c.a), cell(c.b)); got != c.want {
			t.Errorf("%s: conflict %v, want %v", c.name, got, c.want)
		}
	}
}
