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

// fixtureSetup limits a fixture's own steps together: building {bin},
// starting the server and setting it up, and pulling the image. A variable,
// so the tests can shorten it.
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

// setUpFixture builds {bin} and starts the cell's server, with the variables
// of the services the cell needs, within ctx, which holds the fixture's own
// limit. Building {bin} is one of the fixture's steps, so a stalled module
// download leaves the cell not run within it instead of using up the cell's.
func setUpFixture(ctx context.Context, f registry.Fixture, services []string, root, bin, out string, w io.Writer) (fixtureServer, error) {
	if err := buildBin(ctx, root, bin, os.Environ(), w); err != nil {
		return nil, fmt.Errorf("dfs and dfsctl didn't build: %w", err)
	}
	return startFixture(ctx, f, services, bin, out, w)
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
