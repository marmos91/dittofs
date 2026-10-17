package run

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/marmos91/dittofs/test/harness/fixture"
	"github.com/marmos91/dittofs/test/harness/plan"
	"github.com/marmos91/dittofs/test/harness/registry"
)

// fixtureSetup limits a fixture's own steps: building {bin}, and starting the
// server and setting it up. A variable, so the tests can shorten it.
var fixtureSetup = plan.FixtureSetup

// fixtureServer is a running fixture: what it gives the commands, and its
// stop, which reports the server ending before its cell did.
type fixtureServer interface {
	Placeholders() map[string]string
	Stop() error
}

// startFixture starts a cell's server; the tests replace it.
var startFixture = func(ctx context.Context, f registry.Fixture, services []string, bin, out string, w io.Writer) (fixtureServer, error) {
	s, err := fixture.Start(ctx, f, services, bin, out, w)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// setUpFixture builds {bin} and starts the cell's server, within the
// fixture's own limit, with the variables of the services the cell needs. It
// makes one attempt: the server starts inside the fixture, so a retry could
// hide its flakiness. It returns the server, or why the cell doesn't run.
func setUpFixture(ctx context.Context, f registry.Fixture, services []string, root, bin, out string, w io.Writer) (fixtureServer, string) {
	// decision: building {bin} is one of the fixture's own steps, as a
	// package install is, so a stalled module download leaves the cell not
	// run within the limit instead of using up the cell's.
	setup, cancel := context.WithTimeout(ctx, fixtureSetup)
	defer cancel()
	why := func(err error) string {
		switch {
		case ctx.Err() != nil:
			return stopped(ctx)
		case setup.Err() != nil:
			return fmt.Sprintf("the fixture wasn't set up within %s, and isn't retried", fixtureSetup)
		}
		return "the fixture: " + err.Error()
	}
	if err := buildBin(setup, root, bin, os.Environ(), w); err != nil {
		return nil, why(fmt.Errorf("dfs and dfsctl didn't build: %w", err))
	}
	server, err := startFixture(setup, f, services, bin, out, w)
	if err != nil {
		return nil, why(err)
	}
	return server, ""
}

// cellFixture is fixture f with a cell's matrix values filled in.
func cellFixture(f registry.Fixture, vals map[string]string) (registry.Fixture, error) {
	for _, s := range []*string{&f.Server, &f.Profile, &f.NFS.Version, &f.NFS.Options, &f.SMB.Encryption, &f.Shares, &f.Squash, &f.Lease, &f.Reset} {
		v, err := replace(*s, vals, nil, nil)
		if err != nil {
			return f, err
		}
		*s = v
	}
	return f, nil
}
