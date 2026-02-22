package grace

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show grace period status",
	Long: `Display the current NFSv4 grace period status.

Shows whether a grace period is active, time remaining, and client
reclaim progress. The grace period occurs after server restart to
allow clients to reclaim their previously-held state.

Examples:
  # Show status as table
  dfsctl grace status

  # Show status as JSON
  dfsctl grace status -o json

  # Show status as YAML
  dfsctl grace status -o yaml`,
	RunE: runGraceStatus,
}

// graceStatusRenderer renders grace status as a key-value table.
type graceStatusRenderer struct {
	resp *apiclient.GraceStatusResponse
}

// Headers implements output.TableRenderer.
func (g graceStatusRenderer) Headers() []string {
	return []string{"FIELD", "VALUE"}
}

// Rows implements output.TableRenderer.
func (g graceStatusRenderer) Rows() [][]string {
	if !g.resp.Active {
		return [][]string{
			{"Active", "false"},
			{"Message", g.resp.Message},
		}
	}

	return [][]string{
		{"Active", "true"},
		{"Remaining", fmt.Sprintf("%.0fs", g.resp.RemainingSeconds)},
		{"Expected", fmt.Sprintf("%d clients", g.resp.ExpectedClients)},
		{"Reclaimed", fmt.Sprintf("%d clients", g.resp.ReclaimedClients)},
		{"Duration", g.resp.TotalDuration},
		{"Started", g.resp.StartedAt},
	}
}

func runGraceStatus(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		// Grace status is unauthenticated, but we still need a client
		// with the server URL. Try to get one anyway.
		return err
	}

	resp, err := client.GraceStatus()
	if err != nil {
		return fmt.Errorf("failed to get grace status: %w", err)
	}

	format, err := cmdutil.GetOutputFormatParsed()
	if err != nil {
		return err
	}

	switch format {
	case output.FormatJSON:
		return output.PrintJSON(os.Stdout, resp)
	case output.FormatYAML:
		return output.PrintYAML(os.Stdout, resp)
	default:
		if !resp.Active {
			fmt.Println("No active grace period")
			return nil
		}

		fmt.Println()
		fmt.Println("Grace Period Status")
		fmt.Println("===================")
		fmt.Println()
		fmt.Printf("  Active:     true\n")
		fmt.Printf("  Remaining:  %.0fs\n", resp.RemainingSeconds)
		fmt.Printf("  Expected:   %d clients\n", resp.ExpectedClients)
		fmt.Printf("  Reclaimed:  %d clients\n", resp.ReclaimedClients)
		fmt.Printf("  Duration:   %s\n", resp.TotalDuration)
		fmt.Printf("  Started:    %s\n", resp.StartedAt)
		fmt.Println()
		return nil
	}
}
