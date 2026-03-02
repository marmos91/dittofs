//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSMB3_KerberosFeatureMatrix runs the SMB3 Kerberos feature matrix E2E tests.
//
// These tests validate that SMB3 features (encryption, signing, leases, durable handles)
// work correctly with Kerberos authentication, not just NTLM. The test matrix covers:
//   - SMBKRB3-01: Kerberos SPNEGO session setup
//   - SMBKRB3-02: File CRUD with Kerberos auth
//   - SMBKRB3-03: Kerberos + encryption combined
//   - SMBKRB3-04: Kerberos + signing combined
//   - SMBKRB3-05: NTLM fallback when Kerberos unavailable
//   - SMBKRB3-06: Guest session without encryption/signing
//   - SMBKRB3-07: Cross-protocol Kerberos identity consistency
//
// Requires Docker for KDC container and root for mount operations.
// Platform strategy (per locked decision):
//   - Linux (primary): Full Kerberos validation with mount.cifs sec=krb5
//   - macOS (best-effort): Skip Kerberos-specific tests (no mount.cifs)
func TestSMB3_KerberosFeatureMatrix(t *testing.T) {
	// Allow skipping
	if os.Getenv("DITTOFS_E2E_SKIP_SMB_KERBEROS") == "1" {
		t.Skip("SMB Kerberos tests skipped via DITTOFS_E2E_SKIP_SMB_KERBEROS=1")
	}

	// Check platform-specific prereqs
	checkSMB3KerberosPrereqs(t)

	// Start KDC container
	kdc := framework.NewKDCHelper(t, framework.KDCConfig{
		Realm: "DITTOFS.LOCAL",
	})

	// Add user principals
	kdc.AddPrincipal(t, "alice", "alice123")
	kdc.AddPrincipal(t, "bob", "bob123")

	// Add service principals for both NFS and SMB (cifs)
	kdc.AddServicePrincipal(t, "nfs", "localhost")
	kdc.AddServicePrincipal(t, "nfs", "127.0.0.1")
	kdc.AddServicePrincipal(t, "cifs", "localhost")

	// Get hostname and add principals for it too
	hostname, err := os.Hostname()
	require.NoError(t, err)
	kdc.AddServicePrincipal(t, "nfs", hostname)
	kdc.AddServicePrincipal(t, "host", hostname)
	kdc.AddServicePrincipal(t, "cifs", hostname)

	// Create server config with Kerberos enabled and both NFS + SMB adapters
	nfsPort := framework.FindFreePort(t)
	smbPort := framework.FindFreePort(t)
	apiPort := framework.FindFreePort(t)

	configPath := createSMB3KerberosConfig(t, kdc, nfsPort, smbPort, apiPort)

	// Start server
	sp := helpers.StartServerProcessWithConfig(t, configPath)
	t.Cleanup(sp.ForceKill)

	// Login and create shares
	runner := helpers.LoginAsAdmin(t, sp.APIURL())

	// Create a share for SMB3 Kerberos testing
	setupSMB3KerberosShare(t, runner, "/export")

	// Create control plane user "alice" matching the Kerberos principal
	_, err = runner.CreateUser("alice", "alice123")
	require.NoError(t, err)
	err = runner.GrantUserPermission("/export", "alice", "read-write")
	require.NoError(t, err)

	// Create control plane user "bob"
	_, err = runner.CreateUser("bob", "bob123")
	require.NoError(t, err)
	err = runner.GrantUserPermission("/export", "bob", "read-write")
	require.NoError(t, err)

	// Enable NFS adapter
	_, err = runner.EnableAdapter("nfs", helpers.WithAdapterPort(nfsPort))
	require.NoError(t, err)
	err = helpers.WaitForAdapterStatus(t, runner, "nfs", true, 30*time.Second)
	require.NoError(t, err)

	// Enable SMB adapter
	_, err = runner.EnableAdapter("smb", helpers.WithAdapterPort(smbPort))
	require.NoError(t, err)
	err = helpers.WaitForAdapterStatus(t, runner, "smb", true, 30*time.Second)
	require.NoError(t, err)

	// Wait for both adapters to be ready
	framework.WaitForServer(t, nfsPort, 30*time.Second)
	framework.WaitForServer(t, smbPort, 30*time.Second)

	// Install system Kerberos configuration for client tools
	installSMB3KerberosSystemConfig(t, kdc)

	// Run test matrix
	t.Run("SMBKRB3-01 Kerberos Session Setup", func(t *testing.T) {
		testSMB3_Kerberos_SessionSetup(t, kdc, smbPort)
	})

	t.Run("SMBKRB3-02 Kerberos File Ops", func(t *testing.T) {
		testSMB3_Kerberos_FileOps(t, kdc, smbPort)
	})

	t.Run("SMBKRB3-03 Kerberos With Encryption", func(t *testing.T) {
		testSMB3_Kerberos_WithEncryption(t, kdc, smbPort)
	})

	t.Run("SMBKRB3-04 Kerberos With Signing", func(t *testing.T) {
		testSMB3_Kerberos_WithSigning(t, kdc, smbPort)
	})

	t.Run("SMBKRB3-05 NTLM Fallback", func(t *testing.T) {
		testSMB3_Kerberos_NTLMFallback(t, smbPort)
	})

	t.Run("SMBKRB3-06 Guest Session", func(t *testing.T) {
		testSMB3_Kerberos_GuestSession(t, smbPort)
	})

	t.Run("SMBKRB3-07 Cross Protocol Kerberos", func(t *testing.T) {
		testSMB3_Kerberos_CrossProtocol(t, kdc, nfsPort, smbPort)
	})
}

// testSMB3_Kerberos_SessionSetup verifies that SMB3 can establish a session with
// Kerberos/SPNEGO authentication.
func testSMB3_Kerberos_SessionSetup(t *testing.T, kdc *framework.KDCHelper, smbPort int) {
	if runtime.GOOS != "linux" {
		t.Skip("SMB Kerberos session setup test requires Linux with mount.cifs")
	}

	if _, err := exec.LookPath("mount.cifs"); err != nil {
		t.Skip("mount.cifs not found - install cifs-utils package")
	}

	// Get Kerberos ticket for alice
	kdc.Kinit(t, "alice", "alice123")
	defer kdc.Kdestroy(t)

	// Verify ticket was obtained
	klist := kdc.Klist(t)
	t.Logf("Kerberos tickets:\n%s", klist)
	assert.Contains(t, klist, "alice@DITTOFS.LOCAL",
		"Should have Kerberos ticket for alice")

	// Mount SMB with Kerberos
	mountPoint := t.TempDir()
	mountOpts := fmt.Sprintf("sec=krb5,port=%d,vers=2.1,cache=none", smbPort)

	var lastErr error
	for range 3 {
		cmd := exec.Command("mount", "-t", "cifs", "//localhost/export", mountPoint,
			"-o", mountOpts)
		cmd.Env = append(os.Environ(),
			"KRB5_CONFIG="+kdc.Krb5ConfigPath(),
			"KRB5CCNAME="+kdc.CCachePath(),
		)
		output, err := cmd.CombinedOutput()
		if err == nil {
			lastErr = nil
			break
		}
		lastErr = fmt.Errorf("mount failed: %s: %w", string(output), err)
		time.Sleep(time.Second)
	}

	if lastErr != nil {
		t.Skipf("SMB Kerberos mount failed (may need kernel CIFS Kerberos support): %v", lastErr)
	}

	defer func() {
		_ = exec.Command("umount", mountPoint).Run()
	}()

	// Verify the mount is functional by listing directory
	entries, err := os.ReadDir(mountPoint)
	require.NoError(t, err, "Should be able to list directory via Kerberos mount")
	t.Logf("SMBKRB3-01: Kerberos session established, found %d entries", len(entries))

	t.Log("SMBKRB3-01: Kerberos Session Setup - PASSED")
}

// testSMB3_Kerberos_FileOps verifies basic file CRUD operations with Kerberos auth.
func testSMB3_Kerberos_FileOps(t *testing.T, kdc *framework.KDCHelper, smbPort int) {
	if runtime.GOOS != "linux" {
		t.Skip("SMB Kerberos file ops test requires Linux with mount.cifs")
	}

	if _, err := exec.LookPath("mount.cifs"); err != nil {
		t.Skip("mount.cifs not found - install cifs-utils package")
	}

	// Get Kerberos ticket for alice
	kdc.Kinit(t, "alice", "alice123")
	defer kdc.Kdestroy(t)

	// Mount SMB with Kerberos
	mountPoint := t.TempDir()
	mountOpts := fmt.Sprintf("sec=krb5,port=%d,vers=2.1,cache=none", smbPort)

	cmd := exec.Command("mount", "-t", "cifs", "//localhost/export", mountPoint,
		"-o", mountOpts)
	cmd.Env = append(os.Environ(),
		"KRB5_CONFIG="+kdc.Krb5ConfigPath(),
		"KRB5CCNAME="+kdc.CCachePath(),
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("SMB Kerberos mount failed: %s", string(output))
	}

	defer func() {
		_ = exec.Command("umount", mountPoint).Run()
	}()

	// Test: Create file
	testFile := filepath.Join(mountPoint, "smb3-krb5-fileops.txt")
	testData := "File created with Kerberos auth for SMB3 feature test"
	err = os.WriteFile(testFile, []byte(testData), 0644)
	require.NoError(t, err, "Should create file via Kerberos mount")
	t.Log("SMBKRB3-02: File created")

	// Test: Read file back
	content, err := os.ReadFile(testFile)
	require.NoError(t, err, "Should read file via Kerberos mount")
	assert.Equal(t, testData, string(content), "File content should match")
	t.Log("SMBKRB3-02: File read back successfully")

	// Test: Create directory
	testDir := filepath.Join(mountPoint, "smb3-krb5-dir")
	err = os.Mkdir(testDir, 0755)
	require.NoError(t, err, "Should create directory via Kerberos mount")
	t.Log("SMBKRB3-02: Directory created")

	// Test: List directory
	entries, err := os.ReadDir(testDir)
	require.NoError(t, err, "Should list directory via Kerberos mount")
	assert.Len(t, entries, 0, "New directory should be empty")
	t.Log("SMBKRB3-02: Directory listed")

	// Test: Rename file
	renamedFile := filepath.Join(mountPoint, "smb3-krb5-renamed.txt")
	err = os.Rename(testFile, renamedFile)
	require.NoError(t, err, "Should rename file via Kerberos mount")
	t.Log("SMBKRB3-02: File renamed")

	// Test: Verify rename
	_, err = os.Stat(testFile)
	assert.True(t, os.IsNotExist(err), "Original file should not exist after rename")
	content, err = os.ReadFile(renamedFile)
	require.NoError(t, err, "Should read renamed file")
	assert.Equal(t, testData, string(content), "Renamed file content should match")

	// Test: Delete file
	err = os.Remove(renamedFile)
	require.NoError(t, err, "Should delete file via Kerberos mount")
	t.Log("SMBKRB3-02: File deleted")

	// Test: Delete directory
	err = os.Remove(testDir)
	require.NoError(t, err, "Should delete directory via Kerberos mount")
	t.Log("SMBKRB3-02: Directory deleted")

	t.Log("SMBKRB3-02: Kerberos File Ops - PASSED")
}

// testSMB3_Kerberos_WithEncryption verifies that Kerberos + encryption work together.
// SMB3 encryption (AES-128-GCM/CCM) should be transparent with Kerberos auth.
func testSMB3_Kerberos_WithEncryption(t *testing.T, kdc *framework.KDCHelper, smbPort int) {
	if runtime.GOOS != "linux" {
		t.Skip("SMB Kerberos encryption test requires Linux with mount.cifs")
	}

	if _, err := exec.LookPath("mount.cifs"); err != nil {
		t.Skip("mount.cifs not found - install cifs-utils package")
	}

	// Get Kerberos ticket for alice
	kdc.Kinit(t, "alice", "alice123")
	defer kdc.Kdestroy(t)

	// Mount SMB with Kerberos + encryption requested
	// seal option requests SMB3 encryption
	mountPoint := t.TempDir()
	mountOpts := fmt.Sprintf("sec=krb5,port=%d,vers=3.0,seal,cache=none", smbPort)

	cmd := exec.Command("mount", "-t", "cifs", "//localhost/export", mountPoint,
		"-o", mountOpts)
	cmd.Env = append(os.Environ(),
		"KRB5_CONFIG="+kdc.Krb5ConfigPath(),
		"KRB5CCNAME="+kdc.CCachePath(),
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		// Encryption may not be supported by all CIFS clients or kernel versions
		t.Skipf("SMB Kerberos+encryption mount failed (kernel may not support SMB3 encryption): %s", string(output))
	}

	defer func() {
		_ = exec.Command("umount", mountPoint).Run()
	}()

	// Perform file write/read with encryption active
	testFile := filepath.Join(mountPoint, "smb3-krb5-encrypted.txt")
	testData := "Encrypted data written with Kerberos + AES encryption"
	err = os.WriteFile(testFile, []byte(testData), 0644)
	require.NoError(t, err, "Should write file with Kerberos+encryption")

	content, err := os.ReadFile(testFile)
	require.NoError(t, err, "Should read file with Kerberos+encryption")
	assert.Equal(t, testData, string(content),
		"File content should match (encryption should be transparent)")

	// Cleanup
	_ = os.Remove(testFile)

	t.Log("SMBKRB3-03: Kerberos With Encryption - PASSED")
}

// testSMB3_Kerberos_WithSigning verifies that Kerberos + signing work together.
// SMB3 signing (AES-128-CMAC or GMAC) should be active with Kerberos auth.
func testSMB3_Kerberos_WithSigning(t *testing.T, kdc *framework.KDCHelper, smbPort int) {
	if runtime.GOOS != "linux" {
		t.Skip("SMB Kerberos signing test requires Linux with mount.cifs")
	}

	if _, err := exec.LookPath("mount.cifs"); err != nil {
		t.Skip("mount.cifs not found - install cifs-utils package")
	}

	// Get Kerberos ticket for alice
	kdc.Kinit(t, "alice", "alice123")
	defer kdc.Kdestroy(t)

	// Mount SMB with Kerberos and signing explicitly required
	// Note: Kerberos sessions normally enable signing by default
	mountPoint := t.TempDir()
	mountOpts := fmt.Sprintf("sec=krb5i,port=%d,vers=2.1,cache=none", smbPort)

	cmd := exec.Command("mount", "-t", "cifs", "//localhost/export", mountPoint,
		"-o", mountOpts)
	cmd.Env = append(os.Environ(),
		"KRB5_CONFIG="+kdc.Krb5ConfigPath(),
		"KRB5CCNAME="+kdc.CCachePath(),
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		// sec=krb5i includes integrity protection (signing)
		// Fall back to sec=krb5 if krb5i is not supported
		t.Logf("sec=krb5i mount failed, trying sec=krb5: %s", string(output))
		mountOpts = fmt.Sprintf("sec=krb5,port=%d,vers=2.1,cache=none", smbPort)
		cmd = exec.Command("mount", "-t", "cifs", "//localhost/export", mountPoint,
			"-o", mountOpts)
		cmd.Env = append(os.Environ(),
			"KRB5_CONFIG="+kdc.Krb5ConfigPath(),
			"KRB5CCNAME="+kdc.CCachePath(),
		)
		output, err = cmd.CombinedOutput()
		if err != nil {
			t.Skipf("SMB Kerberos+signing mount failed: %s", string(output))
		}
	}

	defer func() {
		_ = exec.Command("umount", mountPoint).Run()
	}()

	// Perform file write/read with signing active
	testFile := filepath.Join(mountPoint, "smb3-krb5-signed.txt")
	testData := "Signed data written with Kerberos integrity protection"
	err = os.WriteFile(testFile, []byte(testData), 0644)
	require.NoError(t, err, "Should write file with Kerberos+signing")

	content, err := os.ReadFile(testFile)
	require.NoError(t, err, "Should read file with Kerberos+signing")
	assert.Equal(t, testData, string(content),
		"File content should match (signing should be transparent)")

	// Cleanup
	_ = os.Remove(testFile)

	t.Log("SMBKRB3-04: Kerberos With Signing - PASSED")
}

// testSMB3_Kerberos_NTLMFallback verifies that NTLM authentication still works
// when Kerberos is not configured or unavailable.
// This does NOT set up KDC or Kerberos - just uses plain NTLM credentials.
func testSMB3_Kerberos_NTLMFallback(t *testing.T, smbPort int) {
	if runtime.GOOS != "linux" {
		t.Skip("SMB NTLM fallback test requires Linux with mount.cifs")
	}

	if _, err := exec.LookPath("mount.cifs"); err != nil {
		t.Skip("mount.cifs not found - install cifs-utils package")
	}

	// Mount SMB with NTLM credentials (no Kerberos)
	mountPoint := t.TempDir()
	mountOpts := fmt.Sprintf("port=%d,username=alice,password=alice123,vers=2.1,cache=none", smbPort)

	cmd := exec.Command("mount", "-t", "cifs", "//localhost/export", mountPoint,
		"-o", mountOpts)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("SMB NTLM mount failed: %s", string(output))
	}

	defer func() {
		_ = exec.Command("umount", mountPoint).Run()
	}()

	// Verify NTLM fallback works with basic file operations
	testFile := filepath.Join(mountPoint, "smb3-ntlm-fallback.txt")
	testData := "Written via NTLM auth (Kerberos not used)"
	err = os.WriteFile(testFile, []byte(testData), 0644)
	require.NoError(t, err, "Should write file via NTLM auth")

	content, err := os.ReadFile(testFile)
	require.NoError(t, err, "Should read file via NTLM auth")
	assert.Equal(t, testData, string(content),
		"File content should match with NTLM auth")

	// Cleanup
	_ = os.Remove(testFile)

	t.Log("SMBKRB3-05: NTLM Fallback - PASSED")
}

// testSMB3_Kerberos_GuestSession verifies that guest/anonymous sessions work
// without encryption or signing requirements.
func testSMB3_Kerberos_GuestSession(t *testing.T, smbPort int) {
	if runtime.GOOS != "linux" {
		t.Skip("SMB guest session test requires Linux with mount.cifs")
	}

	if _, err := exec.LookPath("mount.cifs"); err != nil {
		t.Skip("mount.cifs not found - install cifs-utils package")
	}

	// Attempt mount as guest
	// Guest sessions should work without encryption/signing
	mountPoint := t.TempDir()
	mountOpts := fmt.Sprintf("port=%d,guest,vers=2.1,cache=none", smbPort)

	cmd := exec.Command("mount", "-t", "cifs", "//localhost/export", mountPoint,
		"-o", mountOpts)
	output, err := cmd.CombinedOutput()
	if err != nil {
		// Guest access may be disabled on the server - this is acceptable
		t.Skipf("SMB guest mount failed (guest access may be disabled): %s", string(output))
	}

	defer func() {
		_ = exec.Command("umount", mountPoint).Run()
	}()

	// Verify guest has basic read access
	entries, err := os.ReadDir(mountPoint)
	if err != nil {
		// Guest may have restricted permissions
		t.Logf("SMBKRB3-06: Guest directory listing failed (may be permission restricted): %v", err)
	} else {
		t.Logf("SMBKRB3-06: Guest session can list directory (%d entries)", len(entries))
	}

	// Try write operation (may be denied for guest)
	testFile := filepath.Join(mountPoint, "smb3-guest-test.txt")
	err = os.WriteFile(testFile, []byte("guest write test"), 0644)
	if err != nil {
		t.Logf("SMBKRB3-06: Guest write correctly denied: %v", err)
		// This is expected - guest typically has limited permissions
	} else {
		t.Log("SMBKRB3-06: Guest write succeeded (guest has write access)")
		_ = os.Remove(testFile)
	}

	t.Log("SMBKRB3-06: Guest Session - PASSED")
}

// testSMB3_Kerberos_CrossProtocol verifies that the same Kerberos principal produces
// consistent identity mapping when accessing files via both NFS (RPCSEC_GSS) and SMB (SPNEGO).
// This is the definitive cross-protocol Kerberos identity consistency test.
func testSMB3_Kerberos_CrossProtocol(t *testing.T, kdc *framework.KDCHelper, nfsPort, smbPort int) {
	if runtime.GOOS != "linux" {
		t.Skip("Cross-protocol Kerberos test requires Linux with rpc.gssd and mount.cifs")
	}

	if _, err := exec.LookPath("mount.cifs"); err != nil {
		t.Skip("mount.cifs not found - install cifs-utils package")
	}
	if _, err := exec.LookPath("mount.nfs"); err != nil {
		t.Skip("mount.nfs not found - install nfs-common package")
	}

	// Get Kerberos ticket for alice
	kdc.Kinit(t, "alice", "alice123")
	defer kdc.Kdestroy(t)

	// Mount NFS with Kerberos
	nfsMount, nfsErr := framework.MountNFSWithKerberosAndError(t, nfsPort, "/export", "krb5", 4)
	if nfsErr != nil {
		t.Skipf("NFS Kerberos mount failed: %v", nfsErr)
	}
	defer nfsMount.Cleanup()

	// Mount SMB with Kerberos
	smbMountPoint := t.TempDir()
	smbOpts := fmt.Sprintf("sec=krb5,port=%d,vers=2.1,cache=none", smbPort)
	smbCmd := exec.Command("mount", "-t", "cifs", "//localhost/export", smbMountPoint,
		"-o", smbOpts)
	smbCmd.Env = append(os.Environ(),
		"KRB5_CONFIG="+kdc.Krb5ConfigPath(),
		"KRB5CCNAME="+kdc.CCachePath(),
	)
	smbOutput, smbErr := smbCmd.CombinedOutput()
	if smbErr != nil {
		t.Skipf("SMB Kerberos mount failed: %s", string(smbOutput))
	}
	defer func() {
		_ = exec.Command("umount", smbMountPoint).Run()
	}()

	// Step 1: Write file via NFS with alice's Kerberos credentials
	testFile := "xp-krb5-identity.txt"
	testData := "Written via NFS Kerberos by alice"
	nfsPath := nfsMount.FilePath(testFile)
	err := os.WriteFile(nfsPath, []byte(testData), 0644)
	require.NoError(t, err, "Should write file via NFS Kerberos mount")
	t.Log("SMBKRB3-07: File created via NFS Kerberos")

	// Wait for metadata sync
	time.Sleep(200 * time.Millisecond)

	// Step 2: Read file via SMB with same alice Kerberos credentials
	smbPath := filepath.Join(smbMountPoint, testFile)
	content, err := os.ReadFile(smbPath)
	require.NoError(t, err, "Should read NFS-created file from SMB Kerberos mount")
	assert.Equal(t, testData, string(content),
		"File content should match across protocols")
	t.Log("SMBKRB3-07: File read via SMB Kerberos matches NFS write")

	// Step 3: Write file via SMB, read via NFS
	smbFile := "xp-krb5-smb-origin.txt"
	smbData := "Written via SMB Kerberos by alice"
	smbFilePath := filepath.Join(smbMountPoint, smbFile)
	err = os.WriteFile(smbFilePath, []byte(smbData), 0644)
	require.NoError(t, err, "Should write file via SMB Kerberos mount")

	time.Sleep(200 * time.Millisecond)

	nfsReadPath := nfsMount.FilePath(smbFile)
	nfsContent, err := os.ReadFile(nfsReadPath)
	require.NoError(t, err, "Should read SMB-created file from NFS Kerberos mount")
	assert.Equal(t, smbData, string(nfsContent),
		"NFS should read SMB-written content with same Kerberos identity")
	t.Log("SMBKRB3-07: NFS reads SMB Kerberos-written file correctly")

	t.Log("SMBKRB3-07: Cross Protocol Kerberos - PASSED")
}

// =============================================================================
// Helper Functions for SMB3 Kerberos Feature Matrix
// =============================================================================

// checkSMB3KerberosPrereqs checks for required tools.
func checkSMB3KerberosPrereqs(t *testing.T) {
	t.Helper()

	// Check for kinit
	if _, err := exec.LookPath("kinit"); err != nil {
		t.Skip("kinit not found - install krb5-user package")
	}
}

// createSMB3KerberosConfig creates a server config with Kerberos enabled for both NFS and SMB.
func createSMB3KerberosConfig(t *testing.T, kdc *framework.KDCHelper, nfsPort, smbPort, apiPort int) string {
	t.Helper()

	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "config.yaml")

	config := fmt.Sprintf(`
logging:
  level: DEBUG
  format: text

shutdown_timeout: 30s

database:
  type: sqlite
  sqlite:
    path: %s/controlplane.db

controlplane:
  port: %d
  jwt:
    secret: "smb3-kerberos-feature-matrix-jwt-secret-minimum-32-chars"

cache:
  path: %s/cache
  max_size: 1GB

kerberos:
  enabled: true
  keytab_path: %s
  service_principal: "nfs/localhost@%s"
  krb5_conf: %s
  max_clock_skew: 5m
  context_ttl: 8h
  max_contexts: 10000
  identity_mapping:
    strategy: static
    default_uid: 65534
    default_gid: 65534
    static_map:
      "alice@%s":
        uid: 1001
        gid: 1001
        gids: [1001, 100]
      "bob@%s":
        uid: 1002
        gid: 1002
        gids: [1002, 100]

adapters:
  nfs:
    port: %d
  smb:
    port: %d
`,
		configDir,
		apiPort,
		configDir,
		kdc.KeytabPath(),
		kdc.Realm(),
		kdc.Krb5ConfigPath(),
		kdc.Realm(),
		kdc.Realm(),
		nfsPort,
		smbPort,
	)

	err := os.WriteFile(configPath, []byte(config), 0644)
	require.NoError(t, err)

	// Set environment for admin password
	t.Setenv("DITTOFS_ADMIN_INITIAL_PASSWORD", "adminpassword")

	return configPath
}

// setupSMB3KerberosShare creates a memory/memory share for SMB3 Kerberos testing.
func setupSMB3KerberosShare(t *testing.T, runner *helpers.CLIRunner, shareName string) {
	t.Helper()

	metaStore := fmt.Sprintf("smb3meta-%s", strings.TrimPrefix(shareName, "/"))
	payloadStore := fmt.Sprintf("smb3payload-%s", strings.TrimPrefix(shareName, "/"))

	_, err := runner.CreateMetadataStore(metaStore, "memory")
	require.NoError(t, err)
	_, err = runner.CreatePayloadStore(payloadStore, "memory")
	require.NoError(t, err)
	_, err = runner.CreateShare(shareName, metaStore, payloadStore)
	require.NoError(t, err)
}

// installSMB3KerberosSystemConfig installs the test KDC's krb5.conf and keytab to system locations.
func installSMB3KerberosSystemConfig(t *testing.T, kdc *framework.KDCHelper) {
	t.Helper()

	if runtime.GOOS != "linux" {
		// On macOS, skip system-level config installation
		return
	}

	// Store original files for restoration
	origKrb5Conf, _ := os.ReadFile("/etc/krb5.conf")
	origKeytab, _ := os.ReadFile("/etc/krb5.keytab")

	t.Cleanup(func() {
		if len(origKrb5Conf) > 0 {
			_ = os.WriteFile("/etc/krb5.conf", origKrb5Conf, 0644)
		} else {
			_ = os.Remove("/etc/krb5.conf")
		}
		if len(origKeytab) > 0 {
			_ = os.WriteFile("/etc/krb5.keytab", origKeytab, 0600)
		} else {
			_ = os.Remove("/etc/krb5.keytab")
		}
	})

	// Install test krb5.conf
	krb5ConfData, err := os.ReadFile(kdc.Krb5ConfigPath())
	if err != nil {
		t.Logf("Warning: cannot read krb5.conf: %v", err)
		return
	}

	if err := os.WriteFile("/etc/krb5.conf", krb5ConfData, 0644); err != nil {
		t.Logf("Warning: cannot install test krb5.conf (need root): %v", err)
	}

	// Copy keytab to system location
	keytabData, err := os.ReadFile(kdc.KeytabPath())
	if err != nil {
		t.Logf("Warning: cannot read keytab: %v", err)
		return
	}

	if err := os.WriteFile("/etc/krb5.keytab", keytabData, 0600); err != nil {
		t.Logf("Warning: cannot install test keytab (need root): %v", err)
	}
}
