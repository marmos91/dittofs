//go:build windows

package commands

import "fmt"

// startDaemon is not supported on Windows.
// Use --foreground flag to run the server in the foreground.
func startDaemon() error {
	return fmt.Errorf("daemon mode is not supported on Windows, use --foreground")
}
