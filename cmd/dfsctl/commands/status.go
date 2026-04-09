package commands

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/credentials"
	"github.com/marmos91/dittofs/internal/cli/health"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/internal/cli/timeutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show server status",
	Long: `Display the status of the connected DittoFS server.

This command checks the server health endpoint and displays
status, uptime, and control plane DB reachability.

When authenticated, per-entity status is fetched from the list
endpoints and displayed as a color-coded table.

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
	Server          string `json:"server" yaml:"server"`
	Status          string `json:"status" yaml:"status"`
	Healthy         bool   `json:"healthy" yaml:"healthy"`
	Service         string `json:"service,omitempty" yaml:"service,omitempty"`
	StartedAt       string `json:"started_at,omitempty" yaml:"started_at,omitempty"`
	Uptime          string `json:"uptime,omitempty" yaml:"uptime,omitempty"`
	ControlPlaneDB  string `json:"control_plane_db,omitempty" yaml:"control_plane_db,omitempty"`
	Error           string `json:"error,omitempty" yaml:"error,omitempty"`
	health.Entities `yaml:",inline"`
}

func runStatus(cmd *cobra.Command, args []string) error {
	// Try to get an authenticated client first so we derive the canonical server
	// URL from a single source (handles --server flag, stored context, and token
	// refresh). The health endpoint doesn't require auth, but we need the correct
	// base URL to avoid checking one server while fetching entities from another.
	var apiClient *apiclient.Client
	apiClient, authErr := cmdutil.GetAuthenticatedClient()

	var serverURL string
	if authErr == nil {
		serverURL = apiClient.BaseURL()
	} else {
		// Fall back to stored context for liveness-only display (no entity fetches).
		store, err := credentials.NewStore()
		if err != nil {
			return fmt.Errorf("failed to initialize credential store: %w", err)
		}

		ctx, err := store.GetCurrentContext()
		if err != nil {
			return fmt.Errorf("not logged in. Run 'dfsctl login' first")
		}

		serverURL = ctx.ServerURL
		if serverURL == "" {
			return fmt.Errorf("no server configured. Run 'dfsctl login' first")
		}
	}

	serverURL = strings.TrimRight(serverURL, "/")

	status := ServerStatus{
		Server: serverURL,
		Status: "unreachable",
	}

	// Check health endpoint
	healthURL := serverURL + "/health"
	// 10s timeout: must exceed the server-side 5s HealthCheckTimeout plus network latency.
	client := &http.Client{Timeout: 10 * time.Second}

	resp, err := client.Get(healthURL)
	if err != nil {
		status.Error = err.Error()
	} else {
		defer func() { _ = resp.Body.Close() }()

		var healthResp health.Response
		if err := json.NewDecoder(resp.Body).Decode(&healthResp); err == nil {
			status.Status = healthResp.Status
			status.Healthy = healthResp.Status == "healthy" || healthResp.Status == "degraded"
			status.Service = healthResp.Data.Service
			status.StartedAt = healthResp.Data.StartedAt
			status.Uptime = healthResp.Data.Uptime
			status.ControlPlaneDB = healthResp.Data.ControlPlaneDB
			status.Error = healthResp.Error
		} else {
			status.Status = "unknown"
			status.Error = "Failed to parse health response"
		}
	}

	// Fetch per-entity status when authenticated and server reachable.
	if status.Healthy && apiClient != nil {
		status.Entities = health.FetchEntities(client, apiClient.BaseURL()+"/api/v1", apiClient.Token())
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

	switch {
	case status.Healthy && status.Status != "degraded":
		fmt.Printf("  Status:     \033[32m● %s\033[0m\n", status.Status)
	case status.Status == "unreachable":
		fmt.Printf("  Status:     \033[31m○ %s\033[0m\n", status.Status)
	default:
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
	if status.ControlPlaneDB != "" {
		fmt.Printf("  CP DB:      %s\n", status.ControlPlaneDB)
	}

	health.PrintEntityStatus(status.Entities)

	if status.Error != "" {
		fmt.Printf("  Error:      %s\n", status.Error)
	}
	fmt.Println()
}
