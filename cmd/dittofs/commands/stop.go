package commands

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
)

var (
	stopPidFile string
	stopForce   bool
)

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the DittoFS server",
	Long: `Stop a running DittoFS server.

By default, sends SIGTERM for graceful shutdown. Use --force for immediate
termination with SIGKILL.

Examples:
  # Stop server (uses default PID file)
  dittofs stop

  # Stop server using custom PID file
  dittofs stop --pid-file /var/run/dittofs.pid

  # Force stop (SIGKILL)
  dittofs stop --force`,
	RunE: runStop,
}

func init() {
	stopCmd.Flags().StringVar(&stopPidFile, "pid-file", "", "Path to PID file (default: $XDG_STATE_HOME/dittofs/dittofs.pid)")
	stopCmd.Flags().BoolVarP(&stopForce, "force", "f", false, "Force kill (SIGKILL) instead of graceful shutdown (SIGTERM)")
}

func runStop(cmd *cobra.Command, args []string) error {
	// Use default PID file if not specified
	pidPath := stopPidFile
	if pidPath == "" {
		pidPath = GetDefaultPidFile()
	}

	// Read PID file
	pidData, err := os.ReadFile(pidPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("PID file not found: %s\n\nIs the server running?", pidPath)
		}
		return fmt.Errorf("failed to read PID file: %w", err)
	}

	// Parse PID
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil {
		return fmt.Errorf("invalid PID in file: %s", string(pidData))
	}

	// Find the process
	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("failed to find process %d: %w", pid, err)
	}

	// Send signal
	var sig syscall.Signal
	if stopForce {
		sig = syscall.SIGKILL
		fmt.Printf("Sending SIGKILL to process %d...\n", pid)
	} else {
		sig = syscall.SIGTERM
		fmt.Printf("Sending SIGTERM to process %d...\n", pid)
	}

	if err := process.Signal(sig); err != nil {
		// Check if process already exited
		if err == os.ErrProcessDone {
			fmt.Println("Server already stopped")
			// Clean up PID file
			_ = os.Remove(pidPath)
			return nil
		}
		return fmt.Errorf("failed to send signal: %w", err)
	}

	if stopForce {
		fmt.Println("Server terminated")
	} else {
		fmt.Println("Shutdown signal sent. Server will stop gracefully.")
	}

	return nil
}
