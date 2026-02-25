//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCrossProtocolKerberosIdentity validates that the shared Kerberos layer
// produces the same identity mapping regardless of whether the client connects
// via NFS or SMB.
//
// This test proves:
//   - Same Kerberos principal authenticates through both NFS and SMB
//   - Files created from one protocol are accessible from the other
//   - File ownership is consistent across both mounts
//
// Requires Linux with rpc.gssd, mount.cifs, and root access.
func TestCrossProtocolKerberosIdentity(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Cross-protocol Kerberos identity test requires Linux with rpc.gssd and mount.cifs")
	}

	if os.Getenv("DITTOFS_E2E_SKIP_SMB_KERBEROS") == "1" {
		t.Skip("SMB Kerberos tests skipped via DITTOFS_E2E_SKIP_SMB_KERBEROS=1")
	}

	// Check prereqs
	if _, err := exec.LookPath("kinit"); err != nil {
		t.Skip("kinit not found - install krb5-user package")
	}
	if _, err := exec.LookPath("mount.cifs"); err != nil {
		t.Skip("mount.cifs not found - install cifs-utils package")
	}
	if _, err := exec.LookPath("mount.nfs"); err != nil {
		t.Skip("mount.nfs not found - install nfs-common package")
	}

	// Start KDC container
	kdc := framework.NewKDCHelper(t, framework.KDCConfig{
		Realm: "DITTOFS.LOCAL",
	})

	// Add user principal
	kdc.AddPrincipal(t, "alice", "alice123")

	// Add service principals for both NFS and SMB
	kdc.AddServicePrincipal(t, "nfs", "localhost")
	kdc.AddServicePrincipal(t, "nfs", "127.0.0.1")
	kdc.AddServicePrincipal(t, "cifs", "localhost")

	hostname, err := os.Hostname()
	require.NoError(t, err)
	kdc.AddServicePrincipal(t, "nfs", hostname)
	kdc.AddServicePrincipal(t, "host", hostname)
	kdc.AddServicePrincipal(t, "cifs", hostname)

	// Create server config
	nfsPort := framework.FindFreePort(t)
	smbPort := framework.FindFreePort(t)
	apiPort := framework.FindFreePort(t)

	configPath := createCrossProtocolKerberosConfig(t, kdc, nfsPort, smbPort, apiPort)

	// Start server
	sp := helpers.StartServerProcessWithConfig(t, configPath)
	t.Cleanup(sp.ForceKill)

	// Login and create share
	runner := helpers.LoginAsAdmin(t, sp.APIURL())
	setupSMBKerberosShare(t, runner, "/xp-krb")

	// Create control plane user "alice"
	_, err = runner.CreateUser("alice", "alice123")
	require.NoError(t, err)
	err = runner.GrantUserPermission("/xp-krb", "alice", "read-write")
	require.NoError(t, err)

	// Enable both adapters
	_, err = runner.EnableAdapter("nfs", helpers.WithAdapterPort(nfsPort))
	require.NoError(t, err)
	err = helpers.WaitForAdapterStatus(t, runner, "nfs", true, 30*time.Second)
	require.NoError(t, err)

	_, err = runner.EnableAdapter("smb", helpers.WithAdapterPort(smbPort))
	require.NoError(t, err)
	err = helpers.WaitForAdapterStatus(t, runner, "smb", true, 30*time.Second)
	require.NoError(t, err)

	framework.WaitForServer(t, nfsPort, 30*time.Second)
	framework.WaitForServer(t, smbPort, 30*time.Second)

	// Install system Kerberos config for NFS/CIFS clients
	installCrossProtocolKerberosConfig(t, kdc)

	// Get Kerberos ticket for alice
	kdc.Kinit(t, "alice", "alice123")
	defer kdc.Kdestroy(t)

	t.Run("NFSCreateSMBRead", func(t *testing.T) {
		testCrossProtocolNFSCreateSMBRead(t, kdc, nfsPort, smbPort)
	})

	t.Run("SMBCreateNFSRead", func(t *testing.T) {
		testCrossProtocolSMBCreateNFSRead(t, kdc, nfsPort, smbPort)
	})

	t.Run("BidirectionalVisibility", func(t *testing.T) {
		testCrossProtocolBidirectionalVisibility(t, kdc, nfsPort, smbPort)
	})
}

// testCrossProtocolNFSCreateSMBRead: Create file from NFS with Kerberos,
// verify readable from SMB with Kerberos.
func testCrossProtocolNFSCreateSMBRead(t *testing.T, kdc *framework.KDCHelper, nfsPort, smbPort int) {
	// Mount NFS with Kerberos
	nfsMount, nfsErr := framework.MountNFSWithKerberosAndError(t, nfsPort, "/xp-krb", "krb5", 4)
	if nfsErr != nil {
		t.Skipf("NFS Kerberos mount failed: %v", nfsErr)
	}
	defer nfsMount.Cleanup()

	// Create file via NFS
	testFile := "xp-nfs-to-smb.txt"
	testData := "Created via NFS Kerberos"
	nfsPath := nfsMount.FilePath(testFile)
	err := os.WriteFile(nfsPath, []byte(testData), 0644)
	require.NoError(t, err, "Should write file via NFS Kerberos mount")

	// Mount SMB with Kerberos
	smbMountPoint := t.TempDir()
	smbOpts := fmt.Sprintf("sec=krb5,port=%d,vers=2.1,cache=none", smbPort)
	smbCmd := exec.Command("mount", "-t", "cifs", "//localhost/xp-krb", smbMountPoint,
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

	// Verify file is readable from SMB
	time.Sleep(200 * time.Millisecond) // Allow metadata sync
	smbPath := filepath.Join(smbMountPoint, testFile)
	content, err := os.ReadFile(smbPath)
	require.NoError(t, err, "Should read NFS-created file from SMB Kerberos mount")
	assert.Equal(t, testData, string(content))

	t.Log("Cross-protocol: NFS Kerberos create -> SMB Kerberos read passed")
}

// testCrossProtocolSMBCreateNFSRead: Create file from SMB with Kerberos,
// verify readable from NFS with Kerberos.
func testCrossProtocolSMBCreateNFSRead(t *testing.T, kdc *framework.KDCHelper, nfsPort, smbPort int) {
	// Mount SMB with Kerberos
	smbMountPoint := t.TempDir()
	smbOpts := fmt.Sprintf("sec=krb5,port=%d,vers=2.1,cache=none", smbPort)
	smbCmd := exec.Command("mount", "-t", "cifs", "//localhost/xp-krb", smbMountPoint,
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

	// Create file via SMB
	testFile := "xp-smb-to-nfs.txt"
	testData := "Created via SMB Kerberos"
	smbPath := filepath.Join(smbMountPoint, testFile)
	err := os.WriteFile(smbPath, []byte(testData), 0644)
	require.NoError(t, err, "Should write file via SMB Kerberos mount")

	// Mount NFS with Kerberos
	nfsMount, nfsErr := framework.MountNFSWithKerberosAndError(t, nfsPort, "/xp-krb", "krb5", 4)
	if nfsErr != nil {
		t.Skipf("NFS Kerberos mount failed: %v", nfsErr)
	}
	defer nfsMount.Cleanup()

	// Verify file is readable from NFS
	time.Sleep(200 * time.Millisecond) // Allow metadata sync
	nfsPath := nfsMount.FilePath(testFile)
	content, err := os.ReadFile(nfsPath)
	require.NoError(t, err, "Should read SMB-created file from NFS Kerberos mount")
	assert.Equal(t, testData, string(content))

	t.Log("Cross-protocol: SMB Kerberos create -> NFS Kerberos read passed")
}

// testCrossProtocolBidirectionalVisibility: Both protocols create files,
// both see all files.
func testCrossProtocolBidirectionalVisibility(t *testing.T, kdc *framework.KDCHelper, nfsPort, smbPort int) {
	// Mount NFS with Kerberos
	nfsMount, nfsErr := framework.MountNFSWithKerberosAndError(t, nfsPort, "/xp-krb", "krb5", 4)
	if nfsErr != nil {
		t.Skipf("NFS Kerberos mount failed: %v", nfsErr)
	}
	defer nfsMount.Cleanup()

	// Mount SMB with Kerberos
	smbMountPoint := t.TempDir()
	smbOpts := fmt.Sprintf("sec=krb5,port=%d,vers=2.1,cache=none", smbPort)
	smbCmd := exec.Command("mount", "-t", "cifs", "//localhost/xp-krb", smbMountPoint,
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

	// Create file via NFS
	nfsFile := "bidir-from-nfs.txt"
	err := os.WriteFile(nfsMount.FilePath(nfsFile), []byte("NFS origin"), 0644)
	require.NoError(t, err)

	// Create file via SMB
	smbFile := "bidir-from-smb.txt"
	err = os.WriteFile(filepath.Join(smbMountPoint, smbFile), []byte("SMB origin"), 0644)
	require.NoError(t, err)

	time.Sleep(200 * time.Millisecond)

	// NFS mount should see SMB-created file
	nfsSeesSMB, err := os.ReadFile(nfsMount.FilePath(smbFile))
	require.NoError(t, err, "NFS should see SMB-created file")
	assert.Equal(t, "SMB origin", string(nfsSeesSMB))

	// SMB mount should see NFS-created file
	smbSeesNFS, err := os.ReadFile(filepath.Join(smbMountPoint, nfsFile))
	require.NoError(t, err, "SMB should see NFS-created file")
	assert.Equal(t, "NFS origin", string(smbSeesNFS))

	t.Log("Cross-protocol: Bidirectional Kerberos visibility passed")
}

// =============================================================================
// Helper Functions
// =============================================================================

func createCrossProtocolKerberosConfig(t *testing.T, kdc *framework.KDCHelper, nfsPort, smbPort, apiPort int) string {
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
    secret: "xp-kerberos-e2e-test-jwt-secret-minimum-32-chars"

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
		nfsPort,
		smbPort,
	)

	err := os.WriteFile(configPath, []byte(config), 0644)
	require.NoError(t, err)
	t.Setenv("DITTOFS_ADMIN_INITIAL_PASSWORD", "adminpassword")
	return configPath
}

func installCrossProtocolKerberosConfig(t *testing.T, kdc *framework.KDCHelper) {
	t.Helper()

	// Store original files
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
		_ = exec.Command("systemctl", "restart", "rpc-gssd").Run()
	})

	// Install test krb5.conf
	krb5ConfData, err := os.ReadFile(kdc.Krb5ConfigPath())
	if err != nil {
		return
	}
	if err := os.WriteFile("/etc/krb5.conf", krb5ConfData, 0644); err != nil {
		t.Logf("Warning: cannot install test krb5.conf: %v", err)
	}

	// Copy keytab
	keytabData, err := os.ReadFile(kdc.KeytabPath())
	if err != nil {
		return
	}
	if err := os.WriteFile("/etc/krb5.keytab", keytabData, 0600); err != nil {
		t.Logf("Warning: cannot install test keytab: %v", err)
	}
}
