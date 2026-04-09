package commands

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/marmos91/dittofs/internal/cli/health"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/internal/cli/timeutil"
	"github.com/spf13/cobra"
)

var (
	statusOutput  string
	statusPidFile string
	statusAPIPort int
	statusToken   string
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show server status",
	Long: `Display the current status of the DittoFS server.

This command checks the server health by calling the health endpoint
and displays status, uptime, and control plane DB reachability.

When an API token is provided (--api-token or DFS_API_TOKEN), per-entity
status is fetched from the list endpoints and displayed as a color-coded table.

Examples:
  # Check status (uses default settings)
  dfs status

  # Check status with custom API port
  dfs status --api-port 9080

  # Check status with per-entity details
  dfs status --api-token <token>

  # Output as JSON
  dfs status --output json`,
	RunE: runStatus,
}

func init() {
	statusCmd.Flags().StringVar(&statusPidFile, "pid-file", "", "Path to PID file (default: $XDG_STATE_HOME/dittofs/dittofs.pid)")
	statusCmd.Flags().IntVar(&statusAPIPort, "api-port", 8080, "API server port")
	statusCmd.Flags().StringVarP(&statusOutput, "output", "o", "table", "Output format (table|json|yaml)")
	statusCmd.Flags().StringVar(&statusToken, "api-token", "", "API token for per-entity status (or set DFS_API_TOKEN)")
}

// GracePeriodInfo holds grace period information for the status output.
type GracePeriodInfo struct {
	Active           bool    `json:"active" yaml:"active"`
	RemainingSeconds float64 `json:"remaining_seconds" yaml:"remaining_seconds"`
	ExpectedClients  int     `json:"expected_clients" yaml:"expected_clients"`
	ReclaimedClients int     `json:"reclaimed_clients" yaml:"reclaimed_clients"`
}

// ServerStatus represents the server status information.
type ServerStatus struct {
	Running         bool             `json:"running" yaml:"running"`
	PID             int              `json:"pid,omitempty" yaml:"pid,omitempty"`
	Message         string           `json:"message" yaml:"message"`
	StartedAt       string           `json:"started_at,omitempty" yaml:"started_at,omitempty"`
	Uptime          string           `json:"uptime,omitempty" yaml:"uptime,omitempty"`
	Healthy         bool             `json:"healthy" yaml:"healthy"`
	ControlPlaneDB  string           `json:"control_plane_db,omitempty" yaml:"control_plane_db,omitempty"`
	GracePeriod     *GracePeriodInfo `json:"grace_period,omitempty" yaml:"grace_period,omitempty"`
	health.Entities `yaml:",inline"`
}

func runStatus(cmd *cobra.Command, args []string) error {
	format, err := output.ParseFormat(statusOutput)
	if err != nil {
		return err
	}

	status := ServerStatus{
		Message: "Server is not running",
	}

	// Use default PID file if not specified
	pidPath := statusPidFile
	if pidPath == "" {
		pidPath = GetDefaultPidFile()
	}

	// Check PID file first
	pidData, err := os.ReadFile(pidPath)
	if err == nil {
		pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
		if err == nil {
			// Check if process is running
			process, err := os.FindProcess(pid)
			if err == nil {
				// On Unix, FindProcess always succeeds, we need to send signal 0 to check
				err = process.Signal(syscall.Signal(0))
				if err == nil {
					status.Running = true
					status.PID = pid
				}
			}
		}
	}

	// Check health endpoint (works for both daemon and foreground mode)
	healthURL := fmt.Sprintf("http://localhost:%d/health", statusAPIPort)
	// 10s timeout: must exceed the server-side 5s HealthCheckTimeout plus network latency.
	client := &http.Client{Timeout: 10 * time.Second}

	resp, err := client.Get(healthURL)
	if err == nil {
		defer func() { _ = resp.Body.Close() }()

		var healthResp health.Response
		if err := json.NewDecoder(resp.Body).Decode(&healthResp); err == nil {
			status.Running = true
			status.StartedAt = healthResp.Data.StartedAt
			status.Uptime = healthResp.Data.Uptime
			status.ControlPlaneDB = healthResp.Data.ControlPlaneDB
			switch healthResp.Status {
			case "healthy":
				status.Healthy = true
				status.Message = "Server is running and healthy"
			case "degraded":
				status.Healthy = true // Still operational
				status.Message = "Server is running (degraded: control plane DB unreachable)"
			default:
				status.Message = fmt.Sprintf("Server is running but unhealthy: %s", healthResp.Error)
			}
		} else {
			status.Running = true
			status.Message = "Server is running but health response invalid"
		}
	} else if status.Running {
		// PID file says running but health check failed
		status.Message = "Server process exists but health check failed"
	}

	// Fetch grace period status if server is running
	if status.Running {
		graceURL := fmt.Sprintf("http://localhost:%d/api/v1/grace", statusAPIPort)
		graceResp, graceErr := client.Get(graceURL)
		if graceErr == nil {
			defer func() { _ = graceResp.Body.Close() }()

			var graceInfo GracePeriodInfo
			if json.NewDecoder(graceResp.Body).Decode(&graceInfo) == nil && graceInfo.Active {
				status.GracePeriod = &graceInfo
			}
		}
	}

	// Fetch per-entity status if server is running and token available
	token := statusToken
	if token == "" {
		token = os.Getenv("DFS_API_TOKEN")
	}
	if status.Running && token != "" {
		baseURL := fmt.Sprintf("http://localhost:%d/api/v1", statusAPIPort)
		status.Entities = health.FetchEntities(client, baseURL, token)
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

	if status.Running {
		if status.Healthy {
			fmt.Printf("  Status:     \033[32m● Running\033[0m\n")
		} else {
			fmt.Printf("  Status:     \033[33m● Running (unhealthy)\033[0m\n")
		}
		fmt.Printf("  PID:        %d\n", status.PID)
		if status.StartedAt != "" {
			fmt.Printf("  Started:    %s\n", timeutil.FormatTime(status.StartedAt))
		}
		if status.Uptime != "" {
			fmt.Printf("  Uptime:     %s\n", timeutil.FormatUptime(status.Uptime))
		}
		if status.ControlPlaneDB != "" {
			fmt.Printf("  CP DB:      %s\n", status.ControlPlaneDB)
		}
		if status.GracePeriod != nil && status.GracePeriod.Active {
			remaining := int(status.GracePeriod.RemainingSeconds)
			fmt.Printf("  Grace:      \033[33m%ds remaining (%d/%d clients reclaimed)\033[0m\n",
				remaining, status.GracePeriod.ReclaimedClients, status.GracePeriod.ExpectedClients)
		}
		health.PrintEntityStatus(status.Entities)
	} else {
		fmt.Printf("  Status:     \033[31m○ Stopped\033[0m\n")
	}

	fmt.Println()
	fmt.Printf("  %s\n", status.Message)
	fmt.Println()
}
