//go:build !windows

package commands

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// startDaemon starts the server as a background daemon process.
func startDaemon() error {
	stateDir := GetDefaultStateDir()
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return fmt.Errorf("failed to create state directory: %w", err)
	}

	pidPath := pidFile
	if pidPath == "" {
		pidPath = filepath.Join(stateDir, "dittofs.pid")
	}

	if pid, running := isProcessRunning(pidPath); running {
		return fmt.Errorf("DittoFS is already running (PID %d)\nUse 'dfs stop' to stop the running instance", pid)
	}
	_ = os.Remove(pidPath)

	logPath := logFile
	if logPath == "" {
		logPath = filepath.Join(stateDir, "dittofs.log")
	}

	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %w", err)
	}

	daemonArgs := []string{"start", "--foreground", "--pid-file", pidPath}
	if GetConfigFile() != "" {
		daemonArgs = append(daemonArgs, "--config", GetConfigFile())
	}

	cmd := exec.Command(executable, daemonArgs...)

	logFileHandle, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("failed to open log file: %w", err)
	}

	cmd.Stdout = logFileHandle
	cmd.Stderr = logFileHandle
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}

	if err := cmd.Start(); err != nil {
		_ = logFileHandle.Close()
		return fmt.Errorf("failed to start daemon: %w", err)
	}

	_ = logFileHandle.Close()

	fmt.Printf("DittoFS started in background (PID %d)\n", cmd.Process.Pid)
	fmt.Printf("  PID file: %s\n", pidPath)
	fmt.Printf("  Log file: %s\n", logPath)
	fmt.Println("\nUse 'dfs stop' to stop the server")
	fmt.Println("Use 'dfs status' to check server status")

	return nil
}
