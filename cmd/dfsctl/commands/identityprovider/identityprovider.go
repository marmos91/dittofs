// Package identityprovider implements the `dfsctl identity-provider` command
// group for managing LDAP/AD and Kerberos identity providers over the REST API.
package identityprovider

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
	"os"
)

// Cmd is the parent `identity-provider` command.
var Cmd = &cobra.Command{
	Use:     "identity-provider",
	Aliases: []string{"idp"},
	Short:   "Identity provider (LDAP/AD, Kerberos) management",
	Long: `Manage DittoFS identity providers (LDAP/AD and Kerberos) over the API.

LDAP changes hot-reload the live identity resolver; Kerberos changes take
effect on the next server restart. Secret material (bind password) is
write-only and never displayed.`,
}

func init() {
	Cmd.AddCommand(listCmd)
	Cmd.AddCommand(getCmd)
	Cmd.AddCommand(setCmd)
	Cmd.AddCommand(testCmd)
	Cmd.AddCommand(configureCmd)
}

// --- list ---

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List identity providers and their state",
	RunE:  runList,
}

// ProviderList renders identity-provider summaries as a table.
type ProviderList []apiclient.IdentityProviderSummary

// Headers implements TableRenderer.
func (pl ProviderList) Headers() []string { return []string{"TYPE", "CONFIGURED", "ENABLED"} }

// Rows implements TableRenderer.
func (pl ProviderList) Rows() [][]string {
	rows := make([][]string, 0, len(pl))
	for _, p := range pl {
		rows = append(rows, []string{p.Type, cmdutil.BoolToYesNo(p.Configured), cmdutil.BoolToYesNo(p.Enabled)})
	}
	return rows
}

func runList(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}
	providers, err := client.ListIdentityProviders()
	if err != nil {
		return fmt.Errorf("failed to list identity providers: %w", err)
	}
	return cmdutil.PrintOutput(os.Stdout, providers, len(providers) == 0, "No identity providers.", ProviderList(providers))
}

// --- get ---

var getCmd = &cobra.Command{
	Use:   "get <ldap|kerberos>",
	Short: "Show an identity provider's configuration (secrets redacted)",
	Args:  cobra.ExactArgs(1),
	RunE:  runGet,
}

func runGet(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}
	var cfg any
	switch args[0] {
	case "ldap":
		cfg, err = client.GetLDAPConfig()
	case "kerberos":
		cfg, err = client.GetKerberosConfig()
	default:
		return fmt.Errorf("unknown provider type %q (want ldap or kerberos)", args[0])
	}
	if err != nil {
		return fmt.Errorf("failed to get %s config: %w", args[0], err)
	}
	return cmdutil.PrintResourceWithSuccess(os.Stdout, cfg,
		fmt.Sprintf("Retrieved %s identity provider configuration (use -o json|yaml to view).", args[0]))
}

// --- set ---

var setConfigJSON string

var setCmd = &cobra.Command{
	Use:   "set <ldap|kerberos> --config '<json>'",
	Short: "Create or replace an identity provider's configuration",
	Long: `Create or replace an identity provider's configuration from a JSON body.

The JSON shape matches the API config schema. For LDAP, set "bind_password" to
the real password (or "********" / omit to keep the stored one). LDAP changes
apply live; Kerberos changes apply on the next server restart.

Examples:
  dfsctl identity-provider set ldap --config '{"enabled":true,"url":"ldaps://dc:636","base_dn":"DC=x,DC=y","bind_dn":"CN=svc,DC=x,DC=y","bind_password":"s3cret","idmap":"rfc2307"}'
  dfsctl identity-provider set kerberos --config @/path/to/krb.json`,
	Args: cobra.ExactArgs(1),
	RunE: runSet,
}

func init() {
	setCmd.Flags().StringVar(&setConfigJSON, "config", "", "configuration as a JSON string, or @file to read from a file (required)")
	_ = setCmd.MarkFlagRequired("config")
	testCmd.Flags().StringVar(&testConfigJSON, "config", "", "configuration to test as a JSON string, or @file (required)")
	_ = testCmd.MarkFlagRequired("config")
}

func runSet(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}
	body, err := readConfigArg(setConfigJSON)
	if err != nil {
		return err
	}
	switch args[0] {
	case "ldap":
		var cfg apiclient.LDAPProviderConfig
		if err := json.Unmarshal(body, &cfg); err != nil {
			return fmt.Errorf("invalid LDAP config JSON: %w", err)
		}
		out, err := client.PutLDAPConfig(&cfg)
		if err != nil {
			return fmt.Errorf("failed to set LDAP config: %w", err)
		}
		return cmdutil.PrintResourceWithSuccess(os.Stdout, out, "Updated LDAP identity provider configuration.")
	case "kerberos":
		var cfg apiclient.KerberosProviderConfig
		if err := json.Unmarshal(body, &cfg); err != nil {
			return fmt.Errorf("invalid Kerberos config JSON: %w", err)
		}
		out, err := client.PutKerberosConfig(&cfg)
		if err != nil {
			return fmt.Errorf("failed to set Kerberos config: %w", err)
		}
		if _, perr := fmt.Fprintln(os.Stderr, "Kerberos configuration saved; it will take effect on the next server restart."); perr != nil {
			return perr
		}
		return cmdutil.PrintResourceWithSuccess(os.Stdout, out, "Saved Kerberos identity provider configuration (applies on next server restart).")
	default:
		return fmt.Errorf("unknown provider type %q (want ldap or kerberos)", args[0])
	}
}

// --- test ---

var testConfigJSON string

var testCmd = &cobra.Command{
	Use:   "test <ldap|kerberos> --config '<json>'",
	Short: "Test an identity provider's configuration without persisting it",
	Args:  cobra.ExactArgs(1),
	RunE:  runTest,
}

func runTest(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}
	body, err := readConfigArg(testConfigJSON)
	if err != nil {
		return err
	}
	var result *apiclient.IdentityProviderTestResult
	switch args[0] {
	case "ldap":
		var cfg apiclient.LDAPProviderConfig
		if err := json.Unmarshal(body, &cfg); err != nil {
			return fmt.Errorf("invalid LDAP config JSON: %w", err)
		}
		result, err = client.TestLDAPConfig(&cfg)
	case "kerberos":
		var cfg apiclient.KerberosProviderConfig
		if err := json.Unmarshal(body, &cfg); err != nil {
			return fmt.Errorf("invalid Kerberos config JSON: %w", err)
		}
		result, err = client.TestKerberosConfig(&cfg)
	default:
		return fmt.Errorf("unknown provider type %q (want ldap or kerberos)", args[0])
	}
	if err != nil {
		return fmt.Errorf("failed to test %s config: %w", args[0], err)
	}
	if perr := cmdutil.PrintResourceWithSuccess(os.Stdout, result, fmt.Sprintf("Test result: ok=%t %s", result.OK, result.Message)); perr != nil {
		return perr
	}
	if !result.OK {
		return fmt.Errorf("provider test failed at stage %q: %s", result.Stage, result.Message)
	}
	return nil
}

// --- configure ---

// configureClientFactory is the client constructor used by configureCmd.
// It is a package-level var so tests can substitute a fake client without
// touching the global cobra.Command.
type configureClientFactory func() (*apiclient.Client, error)

var configureCmd = newConfigureCmd(nil) // nil → production default

// newConfigureCmd constructs a fresh configure *cobra.Command. Pass nil for
// clientFn to use the production cmdutil.GetAuthenticatedClient; pass a custom
// function in tests to inject a fake client without reading config files.
func newConfigureCmd(clientFn configureClientFactory) *cobra.Command {
	var (
		machineAccountEnabled bool
		machineAccountName    string
		machineSecret         string
		machineKeytab         string
		dcAddresses           []string
	)

	cmd := &cobra.Command{
		Use:   "configure kerberos",
		Short: "Configure Kerberos machine-account settings",
		Long: `Set individual Kerberos machine-account flags without replacing the full configuration.

The current configuration is read from the API, the specified flags are applied,
and the result is written back. Fields not specified on the command line are
preserved unchanged.

--machine-secret is write-only: omit it to keep the currently stored credential;
provide a new value to rotate it. Submitting the redacted placeholder ("********")
also preserves the stored secret.

Changes take effect on the next server restart.

Examples:
  dfsctl identity-provider configure kerberos --machine-account-enabled --machine-account-name MYHOST$ --machine-secret 'p@ss' --machine-keytab /etc/krb5.keytab --dc-address 192.0.2.10 --dc-address 192.0.2.11`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] != "kerberos" {
				return fmt.Errorf("configure only supports 'kerberos' (got %q)", args[0])
			}

			factory := clientFn
			if factory == nil {
				factory = cmdutil.GetAuthenticatedClient
			}
			client, err := factory()
			if err != nil {
				return err
			}

			// Read the current config so we preserve all existing fields.
			cfg, err := client.GetKerberosConfig()
			if err != nil {
				// Only treat 404 (not yet configured) as an empty starting point.
				// Any other error (403, network, TLS) is a real failure.
				var apiErr *apiclient.APIError
				if !errors.As(err, &apiErr) || !apiErr.IsNotFound() {
					return fmt.Errorf("failed to read current Kerberos config: %w", err)
				}
				cfg = &apiclient.KerberosProviderConfig{}
			}

			// Apply only the flags that were explicitly set.
			if cmd.Flags().Changed("machine-account-enabled") {
				cfg.MachineAccount.Enabled = machineAccountEnabled
			}
			if cmd.Flags().Changed("machine-account-name") {
				cfg.MachineAccount.AccountName = machineAccountName
			}
			if cmd.Flags().Changed("machine-keytab") {
				cfg.MachineAccount.KeytabPath = machineKeytab
			}
			if cmd.Flags().Changed("dc-address") {
				cfg.MachineAccount.DCAddresses = dcAddresses
			}
			// --machine-secret: write-only. Only propagate when the flag was
			// explicitly provided. An absent flag leaves the field empty so the
			// API's preserve-on-empty rule retains the stored credential.
			if cmd.Flags().Changed("machine-secret") {
				cfg.MachineAccount.Secret = machineSecret
			}

			out, err := client.PutKerberosConfig(cfg)
			if err != nil {
				return fmt.Errorf("failed to configure Kerberos: %w", err)
			}
			if _, perr := fmt.Fprintln(os.Stderr, "Kerberos configuration saved; it will take effect on the next server restart."); perr != nil {
				return perr
			}
			return cmdutil.PrintResourceWithSuccess(os.Stdout, out, "Saved Kerberos identity provider configuration (applies on next server restart).")
		},
	}

	cmd.Flags().BoolVar(&machineAccountEnabled, "machine-account-enabled", false,
		"Enable machine-account authentication for NETLOGON")
	cmd.Flags().StringVar(&machineAccountName, "machine-account-name", "",
		"Machine account name (e.g. MYHOST$)")
	cmd.Flags().StringVar(&machineSecret, "machine-secret", "",
		"Machine account password (write-only; omit to keep the stored value)")
	cmd.Flags().StringVar(&machineKeytab, "machine-keytab", "",
		"Path to the machine-account keytab file")
	cmd.Flags().StringArrayVar(&dcAddresses, "dc-address", nil,
		"Domain controller address (repeatable; pass once per address)")

	return cmd
}

// --- helpers ---

// readConfigArg returns the raw config bytes from a JSON string, or from a file
// when the argument is prefixed with '@'.
func readConfigArg(arg string) ([]byte, error) {
	if len(arg) > 1 && arg[0] == '@' {
		data, err := os.ReadFile(arg[1:])
		if err != nil {
			return nil, fmt.Errorf("read config file: %w", err)
		}
		return data, nil
	}
	return []byte(arg), nil
}
