package run

import (
	"context"
	"io"

	"github.com/marmos91/dittofs/test/harness/fixture"
	"github.com/marmos91/dittofs/test/harness/plan"
	"github.com/marmos91/dittofs/test/harness/registry"
)

// fixtureSetup limits a fixture's own steps together: building {bin},
// starting the server and setting it up, and pulling the image. A variable,
// so the tests can shorten it.
var fixtureSetup = plan.FixtureSetup

// fixtureServer is a running fixture: what it gives the commands, the reset
// of a test's share, and its stop, which reports the server ending before
// its cell did.
type fixtureServer interface {
	Placeholders() map[string]string
	Reset(share string) error
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
