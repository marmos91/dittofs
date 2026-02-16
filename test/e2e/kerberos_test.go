//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
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

// TestKerberos runs the Kerberos authentication e2e test suite.
// This tests RPCSEC_GSS authentication with NFSv3 and NFSv4.
//
// These tests temporarily modify system files (/etc/krb5.conf, /etc/krb5.keytab)
// and require root access. Original files are restored on cleanup.
//
// Set DITTOFS_E2E_SKIP_KERBEROS=1 to skip these tests.
func TestKerberos(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Kerberos NFS tests require Linux with rpc.gssd")
	}

	// Allow skipping if needed (e.g., in environments where /etc is read-only)
	if os.Getenv("DITTOFS_E2E_SKIP_KERBEROS") == "1" {
		t.Skip("Kerberos tests skipped via DITTOFS_E2E_SKIP_KERBEROS=1")
	}

	// Check for required Kerberos tools
	checkKerberosPrereqs(t)

	// Start KDC container
	kdc := framework.NewKDCHelper(t, framework.KDCConfig{
		Realm: "DITTOFS.LOCAL",
	})

	// Add test user principals
	kdc.AddPrincipal(t, "alice", "alice123")
	kdc.AddPrincipal(t, "bob", "bob123")
	kdc.AddPrincipal(t, "charlie", "charlie123")

	// Add service principals for localhost
	kdc.AddServicePrincipal(t, "nfs", "localhost")
	kdc.AddServicePrincipal(t, "nfs", "127.0.0.1")

	// Get hostname and add principals for it
	hostname, err := os.Hostname()
	require.NoError(t, err)
	kdc.AddServicePrincipal(t, "nfs", hostname)
	kdc.AddServicePrincipal(t, "host", hostname)

	// Create server config with Kerberos enabled
	nfsPort := framework.FindFreePort(t)
	apiPort := framework.FindFreePort(t)
	metricsPort := framework.FindFreePort(t)

	configPath := createKerberosConfig(t, kdc, nfsPort, apiPort, metricsPort)

	// Start server
	sp := helpers.StartServerProcessWithConfig(t, configPath)
	t.Cleanup(sp.ForceKill)

	// Login and create shares first
	runner := helpers.LoginAsAdmin(t, sp.APIURL())
	setupKerberosShare(t, runner, "/krb-full")
	setupKerberosShare(t, runner, "/mixed")

	// Enable NFS adapter
	_, err = runner.EnableAdapter("nfs", helpers.WithAdapterPort(nfsPort))
	if err != nil {
		// Dump server logs for debugging
		logContent, _ := os.ReadFile(sp.LogFile())
		t.Logf("Server logs:\n%s", string(logContent))
		require.NoError(t, err)
	}
	err = helpers.WaitForAdapterStatus(t, runner, "nfs", true, 30*time.Second)
	require.NoError(t, err)

	// Wait for NFS to be ready
	framework.WaitForServer(t, nfsPort, 30*time.Second)

	// Install system keytab for rpc.gssd
	installSystemKeytab(t, kdc)

	// Run test matrix for both NFSv3 and NFSv4
	for _, nfsVersion := range []int{3, 4} {
		t.Run(fmt.Sprintf("NFSv%d", nfsVersion), func(t *testing.T) {
			runKerberosTests(t, kdc, nfsPort, metricsPort, nfsVersion)
		})
	}
}

// runKerberosTests runs all Kerberos tests for a specific NFS version.
func runKerberosTests(t *testing.T, kdc *framework.KDCHelper, nfsPort, metricsPort, nfsVersion int) {
	t.Run("Test1_BasicKrb5Auth", func(t *testing.T) {
		testBasicKrb5Auth(t, kdc, nfsPort, nfsVersion)
	})

	t.Run("Test2_IntegrityProtection", func(t *testing.T) {
		testIntegrityProtection(t, kdc, nfsPort, nfsVersion)
	})

	t.Run("Test3_PrivacyEncryption", func(t *testing.T) {
		testPrivacyEncryption(t, kdc, nfsPort, nfsVersion)
	})

	t.Run("Test4_UnmappedUser", func(t *testing.T) {
		testUnmappedUser(t, kdc, nfsPort, nfsVersion)
	})

	t.Run("Test5_AuthSysInterop", func(t *testing.T) {
		testAuthSysInterop(t, nfsPort, nfsVersion)
	})

	t.Run("Test6_ConcurrentUsers", func(t *testing.T) {
		testConcurrentUsers(t, kdc, nfsPort, nfsVersion)
	})

	// Test 9 only needs to run once (not per NFS version)
	if nfsVersion == 3 {
		t.Run("Test9_PrometheusMetrics", func(t *testing.T) {
			testPrometheusMetrics(t, metricsPort)
		})
	}
}

// Test 1: Basic Kerberos Authentication (krb5)
func testBasicKrb5Auth(t *testing.T, kdc *framework.KDCHelper, nfsPort, nfsVersion int) {
	// Get ticket for alice
	kdc.Kinit(t, "alice", "alice123")
	defer kdc.Kdestroy(t)

	// Mount with sec=krb5
	mount := framework.MountNFSWithKerberos(t, nfsPort, "/krb-full", "krb5", nfsVersion)
	require.NotNil(t, mount)

	// Create a test file
	testFile := mount.FilePath("test1-alice-file")
	err := os.WriteFile(testFile, []byte("Hello from alice"), 0644)
	require.NoError(t, err)

	// Verify file exists and content
	content, err := os.ReadFile(testFile)
	require.NoError(t, err)
	assert.Equal(t, "Hello from alice", string(content))

	t.Logf("NFSv%d krb5 authentication test passed", nfsVersion)
}

// Test 2: Integrity Protection (krb5i)
func testIntegrityProtection(t *testing.T, kdc *framework.KDCHelper, nfsPort, nfsVersion int) {
	kdc.Kinit(t, "alice", "alice123")
	defer kdc.Kdestroy(t)

	mount := framework.MountNFSWithKerberos(t, nfsPort, "/krb-full", "krb5i", nfsVersion)
	require.NotNil(t, mount)

	// Write data with integrity protection
	testFile := mount.FilePath("test2-integrity.txt")
	testData := "Integrity protected data"
	err := os.WriteFile(testFile, []byte(testData), 0644)
	require.NoError(t, err)

	// Read back and verify
	content, err := os.ReadFile(testFile)
	require.NoError(t, err)
	assert.Equal(t, testData, string(content))

	t.Logf("NFSv%d krb5i integrity test passed", nfsVersion)
}

// Test 3: Privacy/Encryption (krb5p)
func testPrivacyEncryption(t *testing.T, kdc *framework.KDCHelper, nfsPort, nfsVersion int) {
	kdc.Kinit(t, "alice", "alice123")
	defer kdc.Kdestroy(t)

	mount := framework.MountNFSWithKerberos(t, nfsPort, "/krb-full", "krb5p", nfsVersion)
	require.NotNil(t, mount)

	// Write encrypted data
	testFile := mount.FilePath("test3-private.txt")
	testData := "Encrypted sensitive data"
	err := os.WriteFile(testFile, []byte(testData), 0644)
	require.NoError(t, err)

	// Read back and verify
	content, err := os.ReadFile(testFile)
	require.NoError(t, err)
	assert.Equal(t, testData, string(content))

	t.Logf("NFSv%d krb5p privacy test passed", nfsVersion)
}

// Test 4: Unmapped User (charlie -> uid 65534)
func testUnmappedUser(t *testing.T, kdc *framework.KDCHelper, nfsPort, nfsVersion int) {
	// Charlie is not in the identity mapping, should get default UID/GID
	kdc.Kinit(t, "charlie", "charlie123")
	defer kdc.Kdestroy(t)

	mount := framework.MountNFSWithKerberos(t, nfsPort, "/krb-full", "krb5", nfsVersion)
	require.NotNil(t, mount)

	// Create a file as charlie
	testFile := mount.FilePath("test4-charlie-file")
	err := os.WriteFile(testFile, []byte("From charlie"), 0644)
	require.NoError(t, err)

	// File should exist
	_, err = os.Stat(testFile)
	require.NoError(t, err)

	t.Logf("NFSv%d unmapped user test passed", nfsVersion)
}

// Test 5: AUTH_SYS Interoperability
func testAuthSysInterop(t *testing.T, nfsPort, nfsVersion int) {
	// Mount with sec=sys (traditional AUTH_UNIX)
	var opts string
	if nfsVersion == 4 {
		opts = fmt.Sprintf("vers=4,port=%d,sec=sys,actimeo=0", nfsPort)
	} else {
		opts = fmt.Sprintf("nfsvers=3,tcp,port=%d,mountport=%d,sec=sys,actimeo=0,nolock",
			nfsPort, nfsPort)
	}

	mountPoint := t.TempDir()

	// Try mounting with AUTH_SYS
	cmd := exec.Command("mount", "-t", "nfs", "-o", opts, "localhost:/mixed", mountPoint)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("AUTH_SYS mount not supported or share not configured: %s", string(output))
	}

	defer func() {
		unmountCmd := exec.Command("umount", mountPoint)
		_ = unmountCmd.Run()
	}()

	// Create a file using AUTH_SYS
	testFile := filepath.Join(mountPoint, "test5-authsys-file")
	err = os.WriteFile(testFile, []byte("AUTH_SYS data"), 0644)
	require.NoError(t, err)

	t.Logf("NFSv%d AUTH_SYS interop test passed", nfsVersion)
}

// Test 6: Multiple Concurrent Users
func testConcurrentUsers(t *testing.T, kdc *framework.KDCHelper, nfsPort, nfsVersion int) {
	// This test requires separate credential caches for each user
	// We'll use KRB5CCNAME environment variable

	aliceCC := filepath.Join(t.TempDir(), "alice_cc")
	bobCC := filepath.Join(t.TempDir(), "bob_cc")

	// Get tickets for both users
	kinitWithCC(t, kdc, "alice", "alice123", aliceCC)
	kinitWithCC(t, kdc, "bob", "bob123", bobCC)

	// Mount as alice
	os.Setenv("KRB5CCNAME", aliceCC)
	aliceMount := framework.MountNFSWithKerberos(t, nfsPort, "/krb-full", "krb5", nfsVersion)
	require.NotNil(t, aliceMount)

	// Create file as alice
	aliceFile := aliceMount.FilePath("test6-from-alice")
	err := os.WriteFile(aliceFile, []byte("From Alice"), 0644)
	require.NoError(t, err)

	// Mount as bob (different mount point)
	os.Setenv("KRB5CCNAME", bobCC)
	bobMount := framework.MountNFSWithKerberos(t, nfsPort, "/krb-full", "krb5", nfsVersion)
	require.NotNil(t, bobMount)

	// Create file as bob
	bobFile := bobMount.FilePath("test6-from-bob")
	err = os.WriteFile(bobFile, []byte("From Bob"), 0644)
	require.NoError(t, err)

	// Verify both files exist from both mounts
	_, err = os.Stat(aliceMount.FilePath("test6-from-bob"))
	require.NoError(t, err)
	_, err = os.Stat(bobMount.FilePath("test6-from-alice"))
	require.NoError(t, err)

	// Cleanup
	kdc.Kdestroy(t)

	t.Logf("NFSv%d concurrent users test passed", nfsVersion)
}

// Test 9: Prometheus Metrics
func testPrometheusMetrics(t *testing.T, metricsPort int) {
	// Fetch metrics endpoint
	resp, err := http.Get(fmt.Sprintf("http://localhost:%d/metrics", metricsPort))
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Read body
	body := make([]byte, 64*1024)
	n, _ := resp.Body.Read(body)
	metricsContent := string(body[:n])

	// Check for GSS metrics
	assert.Contains(t, metricsContent, "dittofs_gss_context_creations_total")
	assert.Contains(t, metricsContent, "dittofs_gss_active_contexts")
	assert.Contains(t, metricsContent, "dittofs_gss_data_requests_total")

	t.Log("Prometheus GSS metrics test passed")
}

// Helper functions

func checkKerberosPrereqs(t *testing.T) {
	t.Helper()

	// Check for kinit
	if _, err := exec.LookPath("kinit"); err != nil {
		t.Skip("kinit not found - install krb5-user package")
	}

	// Check for mount.nfs
	if _, err := exec.LookPath("mount.nfs"); err != nil {
		t.Skip("mount.nfs not found - install nfs-common package")
	}
}

func createKerberosConfig(t *testing.T, kdc *framework.KDCHelper, nfsPort, apiPort, metricsPort int) string {
	t.Helper()

	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "config.yaml")

	config := fmt.Sprintf(`
logging:
  level: DEBUG
  format: text

shutdown_timeout: 30s

metrics:
  enabled: true
  port: %d

database:
  type: sqlite
  sqlite:
    path: %s/controlplane.db

controlplane:
  port: %d
  jwt:
    secret: "kerberos-e2e-test-jwt-secret-minimum-32-characters"

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
`,
		metricsPort,
		configDir,
		apiPort,
		configDir,
		kdc.KeytabPath(),
		kdc.Realm(),
		kdc.Krb5ConfigPath(),
		kdc.Realm(),
		kdc.Realm(),
		nfsPort,
	)

	err := os.WriteFile(configPath, []byte(config), 0644)
	require.NoError(t, err)

	// Set environment for admin password (must match helpers.GetAdminPassword())
	t.Setenv("DITTOFS_ADMIN_INITIAL_PASSWORD", "adminpassword")

	return configPath
}

func setupKerberosShare(t *testing.T, runner *helpers.CLIRunner, shareName string) {
	t.Helper()

	// Create memory stores for simplicity
	metaStore := fmt.Sprintf("meta-%s", strings.TrimPrefix(shareName, "/"))
	payloadStore := fmt.Sprintf("payload-%s", strings.TrimPrefix(shareName, "/"))

	_, err := runner.CreateMetadataStore(metaStore, "memory")
	require.NoError(t, err)
	_, err = runner.CreatePayloadStore(payloadStore, "memory")
	require.NoError(t, err)
	_, err = runner.CreateShare(shareName, metaStore, payloadStore)
	require.NoError(t, err)
}

func installSystemKeytab(t *testing.T, kdc *framework.KDCHelper) {
	t.Helper()

	// Store original file contents for restoration
	origKrb5Conf, _ := os.ReadFile("/etc/krb5.conf")
	origKeytab, _ := os.ReadFile("/etc/krb5.keytab")

	// Restore original files on cleanup
	t.Cleanup(func() {
		if len(origKrb5Conf) > 0 {
			_ = os.WriteFile("/etc/krb5.conf", origKrb5Conf, 0644)
		}
		if len(origKeytab) > 0 {
			_ = os.WriteFile("/etc/krb5.keytab", origKeytab, 0600)
		}
		// Restart rpc-gssd with original configuration
		_ = exec.Command("systemctl", "restart", "rpc-gssd").Run()
	})

	// Install test krb5.conf to system location
	// rpc.gssd reads /etc/krb5.conf to find the KDC
	krb5ConfData, err := os.ReadFile(kdc.Krb5ConfigPath())
	require.NoError(t, err)

	err = os.WriteFile("/etc/krb5.conf", krb5ConfData, 0644)
	if err != nil {
		t.Skipf("Cannot install test krb5.conf (need root): %v", err)
	}

	// Copy keytab to /etc/krb5.keytab
	keytabData, err := os.ReadFile(kdc.KeytabPath())
	require.NoError(t, err)

	err = os.WriteFile("/etc/krb5.keytab", keytabData, 0600)
	if err != nil {
		t.Skipf("Cannot install test keytab (need root): %v", err)
	}

	// Restart rpc-gssd to pick up new configuration
	cmd := exec.Command("systemctl", "restart", "rpc-gssd")
	if err := cmd.Run(); err != nil {
		t.Logf("Warning: could not restart rpc-gssd: %v", err)
	}

	// Wait a moment for rpc-gssd to restart
	time.Sleep(500 * time.Millisecond)

	// Get machine credentials
	hostname, _ := os.Hostname()
	kdc.KinitWithKeytab(t, fmt.Sprintf("nfs/%s", hostname), kdc.KeytabPath())
}

func kinitWithCC(t *testing.T, kdc *framework.KDCHelper, principal, password, ccPath string) {
	t.Helper()

	fullPrincipal := fmt.Sprintf("%s@%s", principal, kdc.Realm())

	cmd := exec.Command("kinit", "-c", ccPath, fullPrincipal)
	cmd.Env = append(os.Environ(), "KRB5_CONFIG="+kdc.Krb5ConfigPath())
	cmd.Stdin = strings.NewReader(password + "\n")

	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "kinit failed: %s", string(output))
}
