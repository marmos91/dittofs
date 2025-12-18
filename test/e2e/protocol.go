//go:build e2e

package e2e

// Protocol represents the file sharing protocol under test
type Protocol string

const (
	ProtocolNFS Protocol = "nfs"
	ProtocolSMB Protocol = "smb"
)

// GetMountPath returns the mount path for TestContext
func (tc *TestContext) GetMountPath() string {
	return tc.MountPath
}

// GetMountPath returns the mount path for SMBTestContext
func (tc *SMBTestContext) GetMountPath() string {
	return tc.MountPath
}
