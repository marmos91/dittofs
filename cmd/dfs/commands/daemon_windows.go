//go:build windows

package commands

import (
	"fmt"
	"os"
	"os/exec"

	"golang.org/x/sys/windows"
)

// isProcessRunning reads a PID from the given file and checks whether
// that process is still alive on Windows.
//
// On Windows, os.FindProcess always succeeds regardless of whether the
// process exists, and Signal(0) is not reliable for liveness checks.
// We open the process with PROCESS_QUERY_LIMITED_INFORMATION to verify
// it actually exists.
func isProcessRunning(pidPath string) (int, bool) {
	pidData, err := os.ReadFile(pidPath)
	if err != nil {
		return 0, false
	}

	var pid int
	if _, err := fmt.Sscanf(string(pidData), "%d", &pid); err != nil {
		return 0, false
	}

	// OpenProcess with PROCESS_QUERY_LIMITED_INFORMATION (0x1000) fails
	// if the process does not exist, giving us an actual liveness check.
	const processQueryLimitedInformation = 0x1000
	handle, err := windows.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return 0, false
	}
	_ = windows.CloseHandle(handle)

	return pid, true
}

// startDaemon starts the server as a background process on Windows.
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

	if err := cmd.Start(); err != nil {
		lf.Close()
		return fmt.Errorf("failed to start daemon: %w", err)
	}

	// Close the log file handle in the parent process.
	// The child process inherits its own file descriptor, so this is safe.
	lf.Close()

	// Write PID file
	if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d", cmd.Process.Pid)), 0644); err != nil {
		return fmt.Errorf("failed to write PID file: %w", err)
	}

	fmt.Printf("DittoFS server started (PID: %d)\n", cmd.Process.Pid)
	fmt.Printf("Log file: %s\n", logPath)
	fmt.Printf("PID file: %s\n", pidPath)

	return nil
}
