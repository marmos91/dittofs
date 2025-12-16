//go:build e2e

package e2e

import (
	"testing"
)

// Protocol represents the file sharing protocol under test
type Protocol string

const (
	ProtocolNFS Protocol = "nfs"
	ProtocolSMB Protocol = "smb"
)

// ProtocolTestContext defines the common interface for all protocol test contexts.
// Both NFS and SMB test contexts implement this interface.
type ProtocolTestContext interface {
	// Path returns the absolute path for a relative path within the mount
	Path(relativePath string) string

	// GetConfig returns the test configuration
	GetConfig() *TestConfig

	// GetPort returns the server port
	GetPort() int

	// CreateTempDir creates a temporary directory and registers it for cleanup
	CreateTempDir(prefix string) string

	// Cleanup unmounts the share and cleans up resources
	Cleanup()

	// MountPath returns the mount point path
	GetMountPath() string
}

// GetMountPath returns the mount path for TestContext (implements ProtocolTestContext)
func (tc *TestContext) GetMountPath() string {
	return tc.MountPath
}

// GetMountPath returns the mount path for SMBTestContext (implements ProtocolTestContext)
func (tc *SMBTestContext) GetMountPath() string {
	return tc.MountPath
}

// ProtocolTestFunc is a test function that works with any protocol context
type ProtocolTestFunc func(t *testing.T, ptc ProtocolTestContext)

// runOnBothProtocols runs a test on both NFS and SMB protocols
func runOnBothProtocols(t *testing.T, testFunc ProtocolTestFunc) {
	t.Helper()

	// Run on NFS
	t.Run("nfs", func(t *testing.T) {
		runOnLocalConfigs(t, func(t *testing.T, tc *TestContext) {
			testFunc(t, tc)
		})
	})

	// Run on SMB
	t.Run("smb", func(t *testing.T) {
		runSMBOnLocalConfigs(t, func(t *testing.T, tc *SMBTestContext) {
			testFunc(t, tc)
		})
	})
}

// runOnBothProtocolsWithAllConfigs runs a test on both protocols with all configurations
func runOnBothProtocolsWithAllConfigs(t *testing.T, testFunc ProtocolTestFunc) {
	t.Helper()

	// Run on NFS
	t.Run("nfs", func(t *testing.T) {
		runOnAllConfigs(t, func(t *testing.T, tc *TestContext) {
			testFunc(t, tc)
		})
	})

	// Run on SMB
	t.Run("smb", func(t *testing.T) {
		runSMBOnAllConfigs(t, func(t *testing.T, tc *SMBTestContext) {
			testFunc(t, tc)
		})
	})
}

// runOnNFSOnly runs a test only on NFS protocol (for NFS-specific features)
func runOnNFSOnly(t *testing.T, testFunc func(t *testing.T, tc *TestContext)) {
	t.Helper()
	runOnLocalConfigs(t, testFunc)
}

// runOnSMBOnly runs a test only on SMB protocol (for SMB-specific features)
func runOnSMBOnly(t *testing.T, testFunc func(t *testing.T, tc *SMBTestContext)) {
	t.Helper()
	runSMBOnLocalConfigs(t, testFunc)
}
