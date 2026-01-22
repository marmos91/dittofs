// Package cmdutil provides shared utilities for dittofsctl commands.
package cmdutil

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/marmos91/dittofs/internal/cli/credentials"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/internal/cli/prompt"
	"github.com/marmos91/dittofs/pkg/apiclient"
)

// Flags stores global flag values accessible by subcommands.
var Flags = &GlobalFlags{}

// GlobalFlags holds the global flag values.
type GlobalFlags struct {
	ServerURL string
	Token     string
	Output    string
	NoColor   bool
	Verbose   bool
}

// GetAuthenticatedClient returns an API client configured from the current context.
// It uses the --server and --token flags if provided, otherwise falls back to stored credentials.
func GetAuthenticatedClient() (*apiclient.Client, error) {
	// Check for explicit flags first
	if Flags.ServerURL != "" && Flags.Token != "" {
		return apiclient.New(Flags.ServerURL).WithToken(Flags.Token), nil
	}

	// Load credential store
	store, err := credentials.NewStore()
	if err != nil {
		return nil, fmt.Errorf("failed to initialize credential store: %w", err)
	}

	// Get current context
	ctx, err := store.GetCurrentContext()
	if err != nil {
		return nil, fmt.Errorf("not logged in. Run 'dittofsctl login' first")
	}

	// Use flag overrides if provided
	url := ctx.ServerURL
	if Flags.ServerURL != "" {
		url = Flags.ServerURL
	}

	tok := ctx.AccessToken
	if Flags.Token != "" {
		tok = Flags.Token
	}

	if url == "" {
		return nil, fmt.Errorf("no server URL configured. Run 'dittofsctl login --server <url>' first")
	}
	if tok == "" {
		return nil, fmt.Errorf("no access token. Run 'dittofsctl login' first")
	}

	return apiclient.New(url).WithToken(tok), nil
}

// GetOutputFormat returns the output format string.
func GetOutputFormat() string {
	return Flags.Output
}

// GetOutputFormatParsed returns the parsed output format.
func GetOutputFormatParsed() (output.Format, error) {
	return output.ParseFormat(Flags.Output)
}

// IsColorDisabled returns whether color output is disabled.
func IsColorDisabled() bool {
	return Flags.NoColor
}

// IsVerbose returns whether verbose output is enabled.
func IsVerbose() bool {
	return Flags.Verbose
}

// PrintOutput prints data in the specified format (JSON, YAML, or table).
// For table format, it displays emptyMsg if data is empty, otherwise uses the tableRenderer.
func PrintOutput(w io.Writer, data any, isEmpty bool, emptyMsg string, tableRenderer output.TableRenderer) error {
	format, err := GetOutputFormatParsed()
	if err != nil {
		return err
	}

	switch format {
	case output.FormatJSON:
		return output.PrintJSON(w, data)
	case output.FormatYAML:
		return output.PrintYAML(w, data)
	default:
		if isEmpty {
			_, _ = fmt.Fprintln(w, emptyMsg)
			return nil
		}
		return output.PrintTable(w, tableRenderer)
	}
}

// PrintSuccess prints a success message if the output format is table.
func PrintSuccess(msg string) {
	format, err := GetOutputFormatParsed()
	if err != nil || format != output.FormatTable {
		return
	}
	printer := output.NewPrinter(os.Stdout, format, !IsColorDisabled())
	printer.Success(msg)
}

// PrintResourceWithSuccess prints a resource in the specified format.
// For table format, it displays a success message. For JSON/YAML, it outputs the resource.
// This is useful for create, update, and similar operations.
func PrintResourceWithSuccess(w io.Writer, data any, successMsg string) error {
	format, err := GetOutputFormatParsed()
	if err != nil {
		return err
	}

	switch format {
	case output.FormatJSON:
		return output.PrintJSON(w, data)
	case output.FormatYAML:
		return output.PrintYAML(w, data)
	default:
		PrintSuccess(successMsg)
		return nil
	}
}

// PrintCreatedResource is an alias for PrintResourceWithSuccess.
// It prints a created resource in the specified format.
func PrintCreatedResource(w io.Writer, data any, successMsg string) error {
	return PrintResourceWithSuccess(w, data, successMsg)
}

// RunDeleteWithConfirmation prompts for confirmation (unless force is true) and runs deleteFn.
func RunDeleteWithConfirmation(resourceType, name string, force bool, deleteFn func() error) error {
	confirmed, err := prompt.ConfirmWithForce(fmt.Sprintf("Delete %s '%s'?", resourceType, name), force)
	if err != nil {
		return err
	}
	if !confirmed {
		fmt.Println("Aborted.")
		return nil
	}

	if err := deleteFn(); err != nil {
		return err
	}

	PrintSuccess(fmt.Sprintf("%s '%s' deleted successfully", resourceType, name))
	return nil
}

// ParseCommaSeparatedList parses a comma-separated string into a slice of trimmed strings.
func ParseCommaSeparatedList(s string) []string {
	if s == "" {
		return nil
	}
	var result []string
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			result = append(result, item)
		}
	}
	return result
}

// BoolToYesNo converts a boolean to "yes" or "no" string.
func BoolToYesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// EmptyOr returns the value if not empty, otherwise returns the fallback.
// Useful for table display where empty fields should show "-".
func EmptyOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
