// Package quota implements per-identity quota management commands for dfsctl.
package quota

import (
	"fmt"

	"github.com/spf13/cobra"
)

// scope constants mirror models.QuotaScope* (kept local to avoid a control-plane
// import from the CLI layer).
const (
	scopeUser        = "user"
	scopeGroup       = "group"
	scopeDefaultUser = "default-user"
)

// isValidScope reports whether s is a supported quota scope.
func isValidScope(s string) bool {
	switch s {
	case scopeUser, scopeGroup, scopeDefaultUser:
		return true
	default:
		return false
	}
}

// resolveID maps the --id flag (default -1 = unset) to the *uint32 identity the
// API expects. user/group require a non-negative id; default-user forbids one.
func resolveID(scope string, id int64) (*uint32, error) {
	if scope == scopeDefaultUser {
		if id >= 0 {
			return nil, fmt.Errorf("--id must not be set for scope default-user")
		}
		return nil, nil
	}
	if id < 0 {
		return nil, fmt.Errorf("--id is required for scope %s", scope)
	}
	if id > int64(^uint32(0)) {
		return nil, fmt.Errorf("--id %d out of range for a 32-bit identity", id)
	}
	v := uint32(id)
	return &v, nil
}

// Cmd is the parent command for per-identity quota management.
var Cmd = &cobra.Command{
	Use:   "quota",
	Short: "Per-identity quota management",
	Long: `Manage per-identity (user/group/default-user) storage quotas on a share.

Quotas bound both bytes and inode (file) count, with optional soft thresholds
and a grace period before a soft threshold is enforced as hard. These operations
require admin privileges.

Examples:
  # List all quotas on a share
  dfsctl quota list /archive

  # Set a per-user quota (uid 1000)
  dfsctl quota set /archive --scope user --id 1000 --limit-bytes 10GiB --limit-files 100000

  # Set the default-user fallback quota (applies to users without an explicit quota)
  dfsctl quota set /archive --scope default-user --limit-bytes 1GiB

  # Set a per-group quota with soft thresholds and a grace period
  dfsctl quota set /archive --scope group --id 2000 --limit-bytes 50GiB --soft-bytes 45GiB --grace-seconds 604800

  # Remove a per-user quota
  dfsctl quota rm /archive --scope user --id 1000`,
}

func init() {
	Cmd.AddCommand(listCmd)
	Cmd.AddCommand(setCmd)
	Cmd.AddCommand(rmCmd)
}
