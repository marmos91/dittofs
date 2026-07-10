// Package netlogon implements the `dfsctl netlogon` command group for inspecting
// and controlling the NETLOGON machine-account (SMB NTLM pass-through) subsystem
// on a running DittoFS server.
package netlogon

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

// Cmd is the parent `netlogon` command.
var Cmd = &cobra.Command{
	Use:   "netlogon",
	Short: "Inspect and control the NETLOGON machine account (SMB NTLM pass-through)",
	Long: `Inspect and control the NETLOGON machine account used for SMB NTLM pass-through
of Active Directory domain users on the running DittoFS server.

When a client connects by a name with no Kerberos SPN (an IP, or the Explorer →
Network discovery name), Windows falls back to NTLM; DittoFS validates that NTLM
response against a domain controller over a NETLOGON secure channel using a
machine account. These commands report the live machine-account / secure-channel
state and force a machine-password rotation.

Unlike 'dfs netlogon test' (a self-contained probe run from the config file),
these talk to the RUNNING server over the API and require admin credentials.

Examples:
  # Show live machine-account and secure-channel state
  dfsctl netlogon status

  # Force a machine-password rotation now (online-join only)
  dfsctl netlogon rotate`,
}

func init() {
	Cmd.AddCommand(statusCmd)
	Cmd.AddCommand(rotateCmd)
}

// --- status ---

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show live NETLOGON machine-account and secure-channel state",
	Long: `Report the live state of the NETLOGON machine account on the running server:
the active provider (offline or online-join), the machine-account name, the
realm / NetBIOS domain / DC binding, whether the account is joined, whether the
secure channel is currently established, and the automatic-rotation schedule.

Examples:
  # Show status as a table
  dfsctl netlogon status

  # Show status as JSON
  dfsctl netlogon status -o json`,
	Args: cobra.NoArgs,
	RunE: runStatus,
}

// statusRenderer renders a NetlogonStatus as a key/value table.
type statusRenderer struct {
	s *apiclient.NetlogonStatus
}

// Headers implements output.TableRenderer.
func (r statusRenderer) Headers() []string { return []string{"FIELD", "VALUE"} }

// Rows implements output.TableRenderer.
func (r statusRenderer) Rows() [][]string {
	s := r.s
	rows := [][]string{
		{"Enabled", cmdutil.BoolToYesNo(s.Enabled)},
		{"Provider", cmdutil.EmptyOr(s.Provider, "-")},
		{"Account", cmdutil.EmptyOr(s.AccountName, "-")},
		{"Realm", cmdutil.EmptyOr(s.Realm, "-")},
		{"NetBIOS domain", cmdutil.EmptyOr(s.NetBIOSDomain, "-")},
		{"DC addresses", cmdutil.EmptyOr(joinDCs(s.DCAddresses), "(discover via DNS SRV)")},
		{"Channel connected", cmdutil.BoolToYesNo(s.ChannelConnected)},
	}
	// "Joined" is only meaningful for the online-join provider (the offline
	// provider has no provisioning step), so present it as n/a otherwise.
	if s.Provider == "online-join" {
		rows = append(rows, []string{"Joined", cmdutil.BoolToYesNo(s.Joined)})
		rows = append(rows, []string{"Rotation enabled", cmdutil.BoolToYesNo(s.RotationEnabled)})
		if s.RotationEnabled {
			rows = append(rows,
				[]string{"Rotation interval", cmdutil.EmptyOr(s.RotationInterval, "-")},
				[]string{"Next rotation", cmdutil.EmptyOr(s.NextRotation, "-")},
			)
		}
		rows = append(rows, []string{"Last rotation", cmdutil.EmptyOr(s.LastRotation, "never")})
	} else {
		rows = append(rows, []string{"Joined", "n/a (offline provider)"})
	}
	return rows
}

func joinDCs(dcs []string) string {
	out := ""
	for i, dc := range dcs {
		if i > 0 {
			out += ", "
		}
		out += dc
	}
	return out
}

func runStatus(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}
	status, err := client.GetNetlogonStatus()
	if err != nil {
		return fmt.Errorf("failed to get netlogon status: %w", err)
	}
	return cmdutil.PrintOutput(os.Stdout, status, false, "", statusRenderer{s: status})
}

// --- rotate ---

var rotateCmd = &cobra.Command{
	Use:   "rotate",
	Short: "Force a machine-account password rotation now",
	Long: `Force an immediate rotation of the machine-account password on the running server.

Rotation applies only to the online-join provider, which owns the machine-password
lifecycle: the new password is set on the domain controller (NetrServerPasswordSet2),
switched in memory, and persisted — keeping the stored secret in sync with the DC.
The offline/static provider owns no password lifecycle, so this command returns an
error there; rotate that secret by updating the machine-account configuration.

Examples:
  # Rotate the machine password now
  dfsctl netlogon rotate`,
	Args: cobra.NoArgs,
	RunE: runRotate,
}

func runRotate(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}
	result, err := client.RotateNetlogon()
	if err != nil {
		return fmt.Errorf("failed to rotate machine password: %w", err)
	}
	cmdutil.PrintSuccess("Machine-account password rotated.")
	return cmdutil.PrintOutput(os.Stdout, result.Status, false, "", statusRenderer{s: &result.Status})
}
