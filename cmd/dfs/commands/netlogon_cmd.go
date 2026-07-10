package commands

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/marmos91/dittofs/pkg/config"
	"github.com/spf13/cobra"
)

// netlogonCmd groups NETLOGON machine-account (NTLM pass-through) operator
// subcommands. Today it holds `test`; richer status/rotation against the running
// server is tracked as a follow-up (it needs control-plane API wiring).
var netlogonCmd = &cobra.Command{
	Use:   "netlogon",
	Short: "Inspect and test NETLOGON machine-account (NTLM pass-through) setup",
	Long: `NETLOGON machine-account tooling for SMB NTLM pass-through of AD domain users.

When a client connects by a name with no Kerberos SPN (an IP, or the Explorer →
Network discovery name), Windows falls back to NTLM. DittoFS validates that NTLM
response against a domain controller over a NETLOGON secure channel, using a
machine (computer) account configured under 'kerberos.machine_account'. Use these
subcommands to verify that setup.`,
}

var netlogonTestCmd = &cobra.Command{
	Use:   "test",
	Short: "Probe the NETLOGON secure channel to the domain controller",
	Long: `Validate that the configured machine account can establish a NETLOGON secure
channel to a domain controller — the channel NTLM pass-through for AD domain users
rides.

It authenticates the machine account and brings up the sealed secure channel (the
same handshake a real domain-user NTLM logon triggers), then tears it down. No user
logon (NetrLogonSamLogon) is performed. Use it to verify machine-account
credentials, DC reachability, and Kerberos configuration before relying on Explorer
double-click / NTLM logons.

This probes the OFFLINE machine-account channel (kerberos.machine_account with an
account_name + secret). Online join provisions the computer object lazily on the
first domain logon against the running server — start the server and check its log
for the join result instead.

Examples:
  # Probe using the default config location
  dfs netlogon test

  # Probe using an explicit config file
  dfs netlogon test --config /etc/dittofs/config.yaml`,
	Args: cobra.NoArgs,
	RunE: runNetlogonTest,
}

func init() {
	netlogonCmd.AddCommand(netlogonTestCmd)
}

// runNetlogonTest loads the config, builds the offline NETLOGON authenticator, and
// probes the secure channel to the DC, reporting the outcome. It refuses the
// online-join path (which would provision the account as a side effect) and points
// the operator at the running server instead.
func runNetlogonTest(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load(GetConfigFile())
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	k := cfg.Kerberos
	ma := k.MachineAccount

	if !ma.Enabled {
		return fmt.Errorf("kerberos.machine_account.enabled is false — NTLM pass-through is not configured")
	}
	if ma.OnlineJoin.Enabled {
		return fmt.Errorf("`netlogon test` probes the offline machine-account channel; online join provisions the computer object on the first domain logon against the running server — start the server and check its log for the join result")
	}

	// nil secret store: the offline path reads the static secret from config and
	// never touches the control-plane store, so this is safe to run alongside a
	// running server (it makes only an outbound probe to the DC, binds no ports).
	auth, _ := buildNetlogonAuthenticator(k, nil)
	if auth == nil {
		return fmt.Errorf("machine-account configuration is incomplete (see the warnings above); NTLM pass-through is disabled")
	}
	defer auth.Close(context.Background())

	dc := "(discover via DNS SRV)"
	if len(ma.DCAddresses) > 0 {
		dc = strings.Join(ma.DCAddresses, ", ")
	}
	cmd.Printf("Probing NETLOGON secure channel to the domain controller...\n")
	cmd.Printf("  account: %s\n  realm:   %s\n  domain:  %s\n  dc:      %s\n\n", ma.AccountName, k.Realm, k.NetBIOSDomain, dc)

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	if err := auth.Probe(ctx); err != nil {
		return fmt.Errorf("secure channel probe FAILED: %w", err)
	}

	cmd.Printf("OK — NETLOGON secure channel established and torn down successfully.\n")
	cmd.Printf("The machine account can validate AD domain-user NTLM logons.\n")
	return nil
}
