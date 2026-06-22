// Package identityprovider implements the `dfsctl identity-provider` command
// group for managing LDAP/AD and Kerberos identity providers over the REST API.
package identityprovider

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
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
