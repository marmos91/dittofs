//go:build !windows

package commands

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// isProcessRunning reads a PID from the given file and checks whether
// that process is still alive. Returns the PID and true if running,
// or 0 and false otherwise.
func isProcessRunning(pidPath string) (int, bool) {
	pidData, err := os.ReadFile(pidPath)
	if err != nil {
		return 0, false
	}

	var pid int
	if _, err := fmt.Sscanf(string(pidData), "%d", &pid); err != nil {
		return 0, false
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		return 0, false
	}

	if err := process.Signal(syscall.Signal(0)); err != nil {
		return 0, false
	}

	return pid, true
}

// startDaemon starts the server as a background daemon process on Unix systems.
func startDaemon() error {
	// Determine PID file path early to check for existing instance
	pidPath := pidFile
	if pidPath == "" {
		pidPath = GetDefaultPidFile()
	}

	// Check for an already-running instance
	if pid, running := isProcessRunning(pidPath); running {
		return fmt.Errorf("DittoFS is already running (PID %d)\nUse 'dfs stop' to stop the running instance", pid)
	}
	_ = os.Remove(pidPath) // Clean up stale PID file

	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %w", err)
	}

	args := []string{"start", "--foreground", "--pid-file", pidPath}
	if GetConfigFile() != "" {
		args = append(args, "--config", GetConfigFile())
	}

	// Determine log file path
	logPath := logFile
	if logPath == "" {
		logPath = GetDefaultLogFile()
	}

	// Ensure state directory exists
	stateDir := GetDefaultStateDir()
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return fmt.Errorf("failed to create state directory: %w", err)
	}

	// Open log file for daemon output
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("failed to open log file %s: %w", logPath, err)
	}

	cmd := exec.Command(executable, args...)
	cmd.Stdout = lf
	cmd.Stderr = lf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		_ = lf.Close()
		return fmt.Errorf("failed to start daemon: %w", err)
	}

	// Close the log file handle in the parent process.
	// The child process inherits its own file descriptor, so this is safe.
	_ = lf.Close()

	// Write PID file
	if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d", cmd.Process.Pid)), 0644); err != nil {
		return fmt.Errorf("failed to write PID file: %w", err)
	}

	fmt.Printf("DittoFS server started (PID: %d)\n", cmd.Process.Pid)
	fmt.Printf("Log file: %s\n", logPath)
	fmt.Printf("PID file: %s\n", pidPath)

	return nil
}
