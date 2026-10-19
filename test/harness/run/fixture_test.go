package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/harness/registry"
)

// fakeServer is a started fixture that gives fixed placeholders, and
// {mount} when mount is set. Each reset says so in the cell's output.
type fakeServer struct {
	stops    int
	resets   int
	err      error // what Stop reports
	resetErr error // what Reset reports
	mount    string
	out      io.Writer // the cell's output
}

func (f *fakeServer) Placeholders() map[string]string {
	vals := map[string]string{"server": "10.0.0.9", "nfs-port": "40001", "smb-port": "40002", "user": "dt-user", "user2": "dt-user2", "password": "s3cret"}
	if f.mount != "" {
		vals["mount"] = f.mount
	}
	return vals
}

func (f *fakeServer) Reset() error {
	f.resets++
	_, _ = fmt.Fprintln(f.out, "[dt] fake reset")
	return f.resetErr
}

func (f *fakeServer) Stop() error {
	f.stops++
	return f.err
}

// asked is a fixture startFixture was asked for, with the services'
// variables it was given.
type asked struct {
	registry.Fixture
	services []string
}

// fakeFixture makes startFixture call start, and returns the fixtures it was
// asked for.
func fakeFixture(t *testing.T, start func(ctx context.Context) (fixtureServer, error)) *[]asked {
	t.Helper()
	var list []asked
	real := startFixture
	startFixture = func(ctx context.Context, f registry.Fixture, services []string, bin, out string, w io.Writer) (fixtureServer, error) {
		list = append(list, asked{f, services})
		s, err := start(ctx)
		if fake, ok := s.(*fakeServer); ok {
			fake.out = w
		}
		return s, err
	}
	t.Cleanup(func() { startFixture = real })
	return &list
}

const fixtureSuite = `suites:
  f:
    cmd: ["echo {server}:{nfs-port} {user}", "test {password} = s3cret"]
    matrix: { p: [memory, badger-s3, postgres-s3] }
    fixture: { profile: "{p}", nfs: export, lease: 30s }
`

// A cell with a fixture has {bin} built, its server started with the cell's
// values before its first test, its placeholders filled, and its server
// stopped after its last test; {password} never reaches the log. An S3
// profile's fixture is given the variables of the S3 service it needs. A
// cell whose fixture dt can't set up yet doesn't run, and starts nothing.
func TestFixtureCell(t *testing.T) {
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { // what the need's check asks
		_, _ = io.WriteString(w, `{"services": {"s3": "running"}}`)
	}))
	t.Cleanup(s3.Close)
	t.Setenv("LOCALSTACK_ENDPOINT", s3.URL)
	server := &fakeServer{}
	asked := fakeFixture(t, func(context.Context) (fixtureServer, error) { return server, nil })
	root, r := repo(t, fixtureSuite, withBins(map[string]string{"go.mod": goModule["go.mod"]}))
	rep, out := runTarget(t, root, r, "f", false)
	if len(rep.Outcomes) != 3 || rep.Outcomes[0].Result != Passed || rep.Outcomes[1].Result != Passed {
		t.Fatalf("outcomes %+v\n%s", rep.Outcomes, out)
	}
	if o := rep.Outcomes[2]; o.Result != NotRun || o.Note != `dt doesn't set up the "postgres-s3" profile yet` {
		t.Errorf("postgres-s3: %+v", o)
	}
	a := *asked
	if len(a) != 2 || a[0].Profile != "memory" || a[0].Lease != "30s" || a[1].Profile != "badger-s3" || server.stops != 2 || server.resets != 0 {
		t.Errorf("fixtures asked for %+v, stopped %d times, reset %d times", a, server.stops, server.resets)
	}
	if len(a) == 2 && !slices.Contains(a[1].services, "LOCALSTACK_ENDPOINT="+s3.URL) {
		t.Errorf("badger-s3's fixture was given %q", a[1].services)
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

// The fixture's own steps share one limit: an image pulled after the server
// started has only what the start left of it, not a limit of its own.
func TestFixtureStepsShareTheLimit(t *testing.T) {
	fixtureSetup = 3 * time.Second
	t.Cleanup(func() { fixtureSetup = 5 * time.Minute })
	var deadline time.Time
	fakeFixture(t, func(ctx context.Context) (fixtureServer, error) {
		deadline, _ = ctx.Deadline()
		time.Sleep(time.Until(deadline) - time.Second) // a slow start leaves the pull a second
		return &fakeServer{}, nil
	})
	root, r := repo(t, `suites:
  f: { image: x, cmd: "true", fixture: { profile: memory } }
`, withBins(map[string]string{"go.mod": goModule["go.mod"]}))
	fakeDocker(t)
	t.Setenv("FAKE_IMAGE", "stall")
	rep, out := runTarget(t, root, r, "f", false)
	if o := rep.Outcomes[0]; o.Result != NotRun || o.Note != "the fixture wasn't set up within 3s, and isn't retried" {
		t.Errorf("outcome %+v\n%s", o, out)
	}
	if late := time.Since(deadline); late > time.Second {
		t.Errorf("the pull ran %s past the fixture's limit", late.Round(time.Millisecond))
	}
}

// A fixture with reset: each has its share reset before each test of its
// cell, and a test whose reset fails doesn't run. The fixture finds its
// shares file from the repository's root, and the commands get {smb-port}
// and {user2}.
func TestFixtureResetEach(t *testing.T) {
	server := &fakeServer{}
	asked := fakeFixture(t, func(context.Context) (fixtureServer, error) { return server, nil })
	root, r := repo(t, `suites:
  r:
    cmd: "echo {test} {smb-port} {user2}"
    tests: [a, b]
    fixture: { profile: memory, smb: true, shares: smb/shares.yaml, reset: each }
`, withBins(map[string]string{"go.mod": goModule["go.mod"], "smb/shares.yaml": "- name: /s\n"}))
	rep, out := runTarget(t, root, r, "r", false)
	var steps []string
	for _, line := range strings.Split(out, "\n") {
		if line == "[dt] fake reset" || strings.HasPrefix(line, "[dt] $ echo") {
			steps = append(steps, line)
		}
	}
	want := []string{"[dt] fake reset", "[dt] $ echo a 40002 dt-user2", "[dt] fake reset", "[dt] $ echo b 40002 dt-user2"}
	if o := rep.Outcomes[0]; o.Result != Passed || !slices.Equal(steps, want) {
		t.Errorf("outcome %+v, steps:\n%s\nwant:\n%s", o, strings.Join(steps, "\n"), strings.Join(want, "\n"))
	}
	if a := *asked; a[0].Shares != filepath.Join(root, "smb", "shares.yaml") {
		t.Errorf("the fixture's shares file: %s", a[0].Shares)
	}

	server.resetErr = errors.New("the API answered 500")
	rep, out = runTarget(t, root, r, "r", false)
	if o := rep.Outcomes[0]; o.Result != NotRun || o.Note != "a: its share wasn't reset: the API answered 500; b: its share wasn't reset: the API answered 500" || strings.Contains(out, "[dt] $ echo") {
		t.Errorf("a failed reset: %+v\n%s", o, out)
	}
}
