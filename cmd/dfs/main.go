// Tolerate X.509 certificates with negative serial numbers. Samba AD-DC
// auto-generates self-signed TLS certs that commonly have a negative serial,
// which Go's crypto/x509 rejects at parse time (since Go 1.23) — before TLS
// verification runs, so ldap.tls.insecure_skip_verify cannot bypass it. This
// bakes the tolerance into the dfs binary so LDAPS against a default Samba
// AD-DC works without a manual GODEBUG env var. See issue #1289. Production
// directories should still use a properly-issued DC certificate.
//
//go:debug x509negativeserial=1
package main

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfs/commands"
)

// Build-time variables injected via ldflags
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	// Set version info for commands package
	commands.Version = version
	commands.Commit = commit
	commands.Date = date

	if err := commands.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
