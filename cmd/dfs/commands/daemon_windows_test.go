//go:build windows

package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestIsProcessRunning_NonexistentFile(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "nonexistent.pid")

	pid, running := isProcessRunning(pidPath)
	if running {
		t.Errorf("isProcessRunning() for nonexistent file: got running=true, want false")
	}
	if pid != 0 {
		t.Errorf("isProcessRunning() for nonexistent file: got pid=%d, want 0", pid)
	}
}

func TestIsProcessRunning_InvalidPID(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "invalid.pid")
	if err := os.WriteFile(pidPath, []byte("notanumber"), 0644); err != nil {
		t.Fatalf("failed to write pid file: %v", err)
	}

	pid, running := isProcessRunning(pidPath)
	if running {
		t.Errorf("isProcessRunning() for invalid PID: got running=true, want false")
	}
	if pid != 0 {
		t.Errorf("isProcessRunning() for invalid PID: got pid=%d, want 0", pid)
	}
}

func TestIsProcessRunning_DeadProcess(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "dead.pid")
	// PID 9999999 is extremely unlikely to be a running process.
	if err := os.WriteFile(pidPath, []byte("9999999"), 0644); err != nil {
		t.Fatalf("failed to write pid file: %v", err)
	}

	pid, running := isProcessRunning(pidPath)
	if running {
		t.Errorf("isProcessRunning() for dead process: got running=true, want false")
	}
	if pid != 0 {
		t.Errorf("isProcessRunning() for dead process: got pid=%d, want 0", pid)
	}
}

func TestIsProcessRunning_CurrentProcess(t *testing.T) {
	currentPID := os.Getpid()
	pidPath := filepath.Join(t.TempDir(), "current.pid")
	if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d", currentPID)), 0644); err != nil {
		t.Fatalf("failed to write pid file: %v", err)
	}

	pid, running := isProcessRunning(pidPath)
	if !running {
		t.Errorf("isProcessRunning() for current process: got running=false, want true")
	}
	if pid != currentPID {
		t.Errorf("isProcessRunning() for current process: got pid=%d, want %d", pid, currentPID)
	}
}
