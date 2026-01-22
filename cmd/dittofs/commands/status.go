package commands

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/spf13/cobra"
)

var statusOutput string

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show server status",
	Long: `Display the current status of the DittoFS server.

This command checks if the server is running by examining the PID file
and verifying the process is alive.

Examples:
  # Check status
  dittofs status --pid-file /var/run/dittofs.pid

  # Output as JSON
  dittofs status --pid-file /var/run/dittofs.pid --output json`,
	RunE: runStatus,
}

var statusPidFile string

func init() {
	statusCmd.Flags().StringVar(&statusPidFile, "pid-file", "", "Path to PID file")
	statusCmd.Flags().StringVarP(&statusOutput, "output", "o", "table", "Output format (table|json|yaml)")
}

// ServerStatus represents the server status information.
type ServerStatus struct {
	Running bool   `json:"running" yaml:"running"`
	PID     int    `json:"pid,omitempty" yaml:"pid,omitempty"`
	Message string `json:"message" yaml:"message"`
}

func runStatus(cmd *cobra.Command, args []string) error {
	format, err := output.ParseFormat(statusOutput)
	if err != nil {
		return err
	}

	status := ServerStatus{
		Running: false,
		Message: "Server is not running",
	}

	if statusPidFile != "" {
		// Check PID file
		pidData, err := os.ReadFile(statusPidFile)
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
						status.Message = "Server is running"
					} else {
						status.Message = "Server is not running (stale PID file)"
					}
				}
			}
		} else if os.IsNotExist(err) {
			status.Message = "PID file not found - server may not be running"
		}
	} else {
		status.Message = "No PID file specified - cannot determine status"
	}

	switch format {
	case output.FormatJSON:
		return output.PrintJSON(os.Stdout, status)
	case output.FormatYAML:
		return output.PrintYAML(os.Stdout, status)
	default:
		printer := output.NewPrinter(os.Stdout, format, true)
		if status.Running {
			printer.Success(fmt.Sprintf("Server is running (PID: %d)", status.PID))
		} else {
			printer.Warning(status.Message)
		}
	}

	return nil
}
