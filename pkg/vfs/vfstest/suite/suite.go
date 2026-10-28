// Package suite is the filesystem service's conformance suite: Run drives any
// Service, a server or a gateway, through what every one must do, one file
// per area of the Service.
package suite

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/vfs"
)

// Run runs the suite against the services newService builds, a fresh one
// for each case.
//
// ponytail: the suite holds only the cases a service passes with nothing
// set up, as no service exists yet to build shares, files or clients for the
// rest. Each area gains its cases, in its own file, with the server that
// first answers them.
func Run(t *testing.T, newService func(t *testing.T) vfs.Service) {
	t.Run("Clients", func(t *testing.T) { clients(t, newService) })
}
