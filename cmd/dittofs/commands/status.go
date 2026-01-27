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
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show server status",
	Long: `Display the current status of the DittoFS server.

This command checks the server health by calling the health endpoint
and displays status, uptime, and store health information.

Examples:
  # Check status (uses default settings)
  dittofs status

  # Check status with custom API port
  dittofs status --api-port 9080

  # Output as JSON
  dittofs status --output json`,
	RunE: runStatus,
}

func init() {
	statusCmd.Flags().StringVar(&statusPidFile, "pid-file", "", "Path to PID file (default: $XDG_STATE_HOME/dittofs/dittofs.pid)")
	statusCmd.Flags().IntVar(&statusAPIPort, "api-port", 8080, "API server port")
	statusCmd.Flags().StringVarP(&statusOutput, "output", "o", "table", "Output format (table|json|yaml)")
}

// ServerStatus represents the server status information.
type ServerStatus struct {
	Running   bool   `json:"running" yaml:"running"`
	PID       int    `json:"pid,omitempty" yaml:"pid,omitempty"`
	Message   string `json:"message" yaml:"message"`
	StartedAt string `json:"started_at,omitempty" yaml:"started_at,omitempty"`
	Uptime    string `json:"uptime,omitempty" yaml:"uptime,omitempty"`
	Healthy   bool   `json:"healthy" yaml:"healthy"`
}

func runStatus(cmd *cobra.Command, args []string) error {
	format, err := output.ParseFormat(statusOutput)
	if err != nil {
		return err
	}

	status := ServerStatus{
		Running: false,
		Healthy: false,
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
	client := &http.Client{Timeout: 2 * time.Second}

	resp, err := client.Get(healthURL)
	if err == nil {
		defer func() { _ = resp.Body.Close() }()

		var healthResp health.Response
		if err := json.NewDecoder(resp.Body).Decode(&healthResp); err == nil {
			status.Running = true
			status.Healthy = healthResp.Status == "healthy"
			status.StartedAt = healthResp.Data.StartedAt
			status.Uptime = healthResp.Data.Uptime
			if status.Healthy {
				status.Message = "Server is running and healthy"
			} else {
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
	} else {
		fmt.Printf("  Status:     \033[31m○ Stopped\033[0m\n")
	}

	fmt.Println()
	fmt.Printf("  %s\n", status.Message)
	fmt.Println()
}
