//go:build windows

package commands

import (
	"fmt"
	"os"
)

// stopProcess terminates the DittoFS server process on Windows.
// Force mode uses process.Kill(); graceful mode sends os.Interrupt.
func stopProcess(process *os.Process, pid int, force bool) error {
	var err error
	if force {
		fmt.Printf("Killing process %d...\n", pid)
		err = process.Kill()
	} else {
		fmt.Printf("Sending interrupt to process %d...\n", pid)
		err = process.Signal(os.Interrupt)
	}

	if err == os.ErrProcessDone {
		return errProcessDone
	}
	if err != nil {
		return fmt.Errorf("failed to stop process: %w", err)
	}

	return nil
}
