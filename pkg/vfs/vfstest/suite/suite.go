// Package suite is the filesystem service's conformance suite: Run drives
// any Service, a filesystem or a gateway, through what every one must do, one
// file per area of the Service.
package suite

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/vfs"
)

// Run runs the suite against the services newService builds: a fresh one,
// serving, for each case.
//
// ponytail: the suite holds only the cases a service passes with nothing set
// up, since the filesystem builds no share, file or client yet. Each area
// gains its cases, in its own file, with the piece of the filesystem that
// first answers them.
func Run(t *testing.T, newService func(t *testing.T) vfs.Service) {
	t.Run("Errors", func(t *testing.T) { neutralErrors(t, newService) })
}
