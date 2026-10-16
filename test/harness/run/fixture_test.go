package run

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/harness/registry"
)

// fakeServer is a started fixture that gives fixed placeholders.
type fakeServer struct {
	stops int
	err   error // what Stop reports
}

func (f *fakeServer) Placeholders() map[string]string {
	return map[string]string{"server": "10.0.0.9", "nfs-port": "40001", "user": "dt-user", "password": "s3cret"}
}

func (f *fakeServer) Stop() error {
	f.stops++
	return f.err
}

// fakeFixture makes startFixture call start, and returns the fixtures it was
// asked for.
func fakeFixture(t *testing.T, start func(ctx context.Context) (fixtureServer, error)) *[]registry.Fixture {
	t.Helper()
	var asked []registry.Fixture
	real := startFixture
	startFixture = func(ctx context.Context, f registry.Fixture, bin, out string, w io.Writer) (fixtureServer, error) {
		asked = append(asked, f)
		return start(ctx)
	}
	t.Cleanup(func() { startFixture = real })
	return &asked
}

const fixtureSuite = `suites:
  f:
    cmd: ["echo {server}:{nfs-port} {user}", "test {password} = s3cret"]
    matrix: { p: [memory, badger-s3] }
    fixture: { profile: "{p}", nfs: export, lease: 30s }
`

// A cell with a fixture has {bin} built, its server started with the cell's
// values before its first test, its placeholders filled, and its server
// stopped after its last test; {password} never reaches the log. A cell whose
// fixture dt can't set up yet doesn't run, and starts nothing.
func TestFixtureCell(t *testing.T) {
	server := &fakeServer{}
	asked := fakeFixture(t, func(context.Context) (fixtureServer, error) { return server, nil })
	root, r := repo(t, fixtureSuite, withBins(map[string]string{"go.mod": goModule["go.mod"]}))
	rep, out := runTarget(t, root, r, "f", false)
	if len(rep.Outcomes) != 2 || rep.Outcomes[0].Result != Passed {
		t.Fatalf("outcomes %+v\n%s", rep.Outcomes, out)
	}
	if o := rep.Outcomes[1]; o.Result != NotRun || o.Note != `dt doesn't set up the "badger-s3" profile yet` {
		t.Errorf("badger-s3: %+v", o)
	}
	if len(*asked) != 1 || (*asked)[0].Profile != "memory" || (*asked)[0].Lease != "30s" || server.stops != 1 {
		t.Errorf("fixtures asked for %+v, stopped %d times", *asked, server.stops)
	}
	for _, bin := range []string{"dfs", "dfsctl"} {
		if _, err := os.Stat(filepath.Join(rep.Dir, "bin", bin)); err != nil {
			t.Errorf("{bin}: %v", err)
		}
	}
	log, _ := os.ReadFile(filepath.Join(rep.Dir, "logs", "f-memory.log"))
	if !strings.Contains(out, "[dt] $ echo 10.0.0.9:40001 dt-user\n") || strings.Contains(string(log), "s3cret") {
		t.Errorf("the command as shown:\n%s\nlog:\n%s", out, log)
	}
}

// A fixture that fails its setup leaves its cell not run and its tests
// unstarted; one that passes the setup's limit is tried once.
func TestFixtureSetupFails(t *testing.T) {
	fakeFixture(t, func(context.Context) (fixtureServer, error) {
		return nil, errors.New("dfs exited before its API answered")
	})
	root, r := repo(t, fixtureSuite, withBins(map[string]string{"go.mod": goModule["go.mod"]}))
	rep, out := runTarget(t, root, r, "f/memory", false)
	if o := rep.Outcomes[0]; o.Result != NotRun || o.Note != "the fixture: dfs exited before its API answered" || strings.Contains(out, "[dt] $ echo") {
		t.Errorf("a failed setup: %+v\n%s", o, out)
	}

	// Long enough for the build, which the first run cached.
	fixtureSetup = 3 * time.Second
	t.Cleanup(func() { fixtureSetup = 5 * time.Minute })
	asked := fakeFixture(t, func(ctx context.Context) (fixtureServer, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	rep, _ = runTarget(t, root, r, "f/memory", false)
	if o := rep.Outcomes[0]; o.Result != NotRun || o.Note != "the fixture wasn't set up within 3s, and isn't retried" || len(*asked) != 1 {
		t.Errorf("a setup past its limit: %+v, started %d times", o, len(*asked))
	}
}

// A server that ended before its cell did fails the cell, whatever its tests
// say.
func TestFixtureServerEnds(t *testing.T) {
	server := &fakeServer{err: errors.New("dfs exited before its cell ended: exit status 2")}
	fakeFixture(t, func(context.Context) (fixtureServer, error) { return server, nil })
	root, r := repo(t, fixtureSuite, withBins(map[string]string{"go.mod": goModule["go.mod"]}))
	rep, _ := runTarget(t, root, r, "f/memory", false)
	if o := rep.Outcomes[0]; o.Result != Failed || o.Note != "the fixture: dfs exited before its cell ended: exit status 2" {
		t.Errorf("outcome %+v", o)
	}
}
