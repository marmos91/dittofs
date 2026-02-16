package commands

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/credentials"
	"github.com/marmos91/dittofs/internal/cli/health"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/internal/cli/timeutil"
	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show server status",
	Long: `Display the status of the connected DittoFS server.

This command checks the server health endpoint and displays
status, uptime, and version information.

Examples:
  # Check status of connected server
  dfsctl status

  # Output as JSON
  dfsctl status -o json`,
	RunE: runStatus,
}

func init() {
	rootCmd.AddCommand(statusCmd)
}

// ServerStatus represents the server status for display.
type ServerStatus struct {
	Server    string `json:"server" yaml:"server"`
	Status    string `json:"status" yaml:"status"`
	Healthy   bool   `json:"healthy" yaml:"healthy"`
	Service   string `json:"service,omitempty" yaml:"service,omitempty"`
	StartedAt string `json:"started_at,omitempty" yaml:"started_at,omitempty"`
	Uptime    string `json:"uptime,omitempty" yaml:"uptime,omitempty"`
	Error     string `json:"error,omitempty" yaml:"error,omitempty"`
}

func runStatus(cmd *cobra.Command, args []string) error {
	// Load credential store
	store, err := credentials.NewStore()
	if err != nil {
		return fmt.Errorf("failed to initialize credential store: %w", err)
	}

	// Get current context
	ctx, err := store.GetCurrentContext()
	if err != nil {
		return fmt.Errorf("not logged in. Run 'dfsctl login' first")
	}

	serverURL := ctx.ServerURL
	if serverURL == "" {
		return fmt.Errorf("no server configured. Run 'dfsctl login' first")
	}

	status := ServerStatus{
		Server:  serverURL,
		Status:  "unreachable",
		Healthy: false,
	}

	// Check health endpoint
	healthURL := serverURL + "/health"
	client := &http.Client{Timeout: 5 * time.Second}

	resp, err := client.Get(healthURL)
	if err != nil {
		status.Error = err.Error()
	} else {
		defer func() { _ = resp.Body.Close() }()

		var healthResp health.Response
		if err := json.NewDecoder(resp.Body).Decode(&healthResp); err == nil {
			status.Status = healthResp.Status
			status.Healthy = healthResp.Status == "healthy"
			status.Service = healthResp.Data.Service
			status.StartedAt = healthResp.Data.StartedAt
			status.Uptime = healthResp.Data.Uptime
			if healthResp.Error != "" {
				status.Error = healthResp.Error
			}
		} else {
			status.Status = "unknown"
			status.Error = "Failed to parse health response"
		}
	}

	// Output based on format
	format, err := cmdutil.GetOutputFormatParsed()
	if err != nil {
		return err
	}

	switch format {
	case output.FormatJSON:
		return output.PrintJSON(os.Stdout, status)
	case output.FormatYAML:
		return output.PrintYAML(os.Stdout, status)
	default:
		printStatusTable(status)
	}

	return nil
}

func printStatusTable(status ServerStatus) {
	fmt.Println()
	fmt.Println("DittoFS Server Status")
	fmt.Println("=====================")
	fmt.Println()
	fmt.Printf("  Server:     %s\n", status.Server)

	if status.Healthy {
		fmt.Printf("  Status:     \033[32m● %s\033[0m\n", status.Status)
	} else if status.Status == "unreachable" {
		fmt.Printf("  Status:     \033[31m○ %s\033[0m\n", status.Status)
	} else {
		fmt.Printf("  Status:     \033[33m● %s\033[0m\n", status.Status)
	}

	if status.Service != "" {
		fmt.Printf("  Service:    %s\n", status.Service)
	}
	if status.StartedAt != "" {
		fmt.Printf("  Started:    %s\n", timeutil.FormatTime(status.StartedAt))
	}
	if status.Uptime != "" {
		fmt.Printf("  Uptime:     %s\n", timeutil.FormatUptime(status.Uptime))
	}
	if status.Error != "" {
		fmt.Printf("  Error:      %s\n", status.Error)
	}
	fmt.Println()
}
