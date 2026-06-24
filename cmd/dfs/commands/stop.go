package commands

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

var (
	stopPidFile string
	stopForce   bool
)

// errProcessDone is a sentinel returned by stopProcess when the process has already exited.
var errProcessDone = errors.New("process already done")

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the DittoFS server",
	Long: `Stop a running DittoFS server.

Sends SIGTERM to the server process identified by the PID file, which triggers
a graceful shutdown: in-flight NFS/SMB requests are drained, snapshot jobs are
flushed, and block stores are closed before the process exits. Use --force to
send SIGKILL instead when a graceful stop is not responding.

Examples:
  # Graceful stop (reads default PID file)
  dfs stop

  # Stop using a custom PID file location
  dfs stop --pid-file /var/run/dittofs.pid

  # Immediately kill the server process
  dfs stop --force`,
	RunE: runStop,
}

func init() {
	stopCmd.Flags().StringVar(&stopPidFile, "pid-file", "", "Path to PID file (default: $XDG_STATE_HOME/dittofs/dittofs.pid)")
	stopCmd.Flags().BoolVarP(&stopForce, "force", "f", false, "Force kill instead of graceful shutdown")
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
		if errors.Is(err, os.ErrNotExist) {
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

	// Send signal (platform-specific)
	if err := stopProcess(process, pid, stopForce); err != nil {
		if errors.Is(err, errProcessDone) {
			fmt.Println("Server already stopped")
			_ = os.Remove(pidPath)
			return nil
		}
		return err
	}

	if stopForce {
		fmt.Println("Server terminated")
	} else {
		fmt.Println("Shutdown signal sent. Server will stop gracefully.")
	}

	return nil
}
