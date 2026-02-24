//go:build !windows

package commands

import (
	"fmt"
	"os"
	"syscall"
)

// stopProcess sends the appropriate signal to stop the DittoFS server process.
func stopProcess(process *os.Process, pid int, force bool) error {
	sig, name := syscall.SIGTERM, "SIGTERM"
	if force {
		sig, name = syscall.SIGKILL, "SIGKILL"
	}

	fmt.Printf("Sending %s to process %d...\n", name, pid)

	err := process.Signal(sig)
	if err == os.ErrProcessDone {
		return errProcessDone
	}
	if err != nil {
		return fmt.Errorf("failed to send signal: %w", err)
	}

	return nil
}
