package run

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/test/harness/report"
)

// A knfsd cell starts the kernel's server, and builds no dfs, which it
// doesn't run: its checkout has none to build. Its commands get {server} and
// {nfs-port}, and none of dfs's users.
func TestKnfsdCell(t *testing.T) {
	asked := fakeFixture(t, func(context.Context) (fixtureServer, error) { return &fakeServer{}, nil })
	root, r := repo(t, `suites:
  k: { cmd: "echo {server}:{nfs-port}", fixture: { server: knfsd, nfs: export } }
  u: { cmd: "echo {user}", fixture: { server: knfsd, nfs: export } }
`, nil)
	var cells []Cell
	for _, name := range []string{"k", "u"} {
		c, err := Select(root, r, name, "", "pr", nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		cells = append(cells, c...)
	}
	for i := range cells {
		cells[i].Needs = nil // the test's, not this machine's knfsd
	}
	var out bytes.Buffer
	rep, err := Run(context.Background(), cells, Options{Root: root, Results: filepath.Join(root, "results"), Out: &out})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Outcomes) != 2 || rep.Outcomes[0].Result != report.Passed || !strings.Contains(out.String(), "[dt] $ echo 10.0.0.9:40001\n") {
		t.Fatalf("outcomes %+v\n%s", rep.Outcomes, out.String())
	}
	if o := rep.Outcomes[1]; o.Result != report.NotRun || !strings.Contains(o.Note, "{user}") {
		t.Errorf("a knfsd cell's command with {user}: %+v", o)
	}
	if a := *asked; len(a) != 1 || a[0].Server != "knfsd" {
		t.Errorf("fixtures asked for %+v", a)
	}
}
