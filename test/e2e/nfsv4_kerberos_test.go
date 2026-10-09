//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// NFSv4 Kerberos v4.0 Mount Helper
// =============================================================================

// krbV4Mount is a simple mount wrapper for Kerberos vers=4.0 mounts.
// We use a local type instead of framework.KerberosMount because the
// framework types have unexported fields that cannot be set from outside
// the framework package.
type krbV4Mount struct {
	t         *testing.T
	path      string
	port      int
	secFlavor string
}

// FilePath returns the absolute path for a relative path within the mount.
func (m *krbV4Mount) FilePath(relativePath string) string {
	return filepath.Join(m.path, relativePath)
}

// Cleanup unmounts and removes the mount directory.
func (m *krbV4Mount) Cleanup() {
	if m.path == "" {
		return
	}
	// Unmount
	cmd := exec.Command("umount", m.path)
	if output, err := cmd.CombinedOutput(); err != nil {
		m.t.Logf("Unmount %s failed (trying force): %v, output: %s", m.path, err, string(output))
		forceCmd := exec.Command("umount", "-f", m.path)
		_ = forceCmd.Run()
	}
	// Remove directory
	_ = os.RemoveAll(m.path)
}

// mountNFSv40WithKerberos mounts an NFS share with Kerberos authentication
// using vers=4.0 explicitly (NOT vers=4). This is the PRIMARY way to mount
// NFSv4.0 with Kerberos in this test file per locked decision #5.
// secFlavor should be "krb5", "krb5i", or "krb5p".
func mountNFSv40WithKerberos(t *testing.T, port int, export, secFlavor string) *krbV4Mount {
	t.Helper()

	if export == "" {
		export = "/export"
	}

	// File operations run as non-root uids, which cannot traverse the 0700
	// directories t.TempDir creates.
	mountPoint, err := os.MkdirTemp("/tmp", "dittofs-e2e-krb-v4-")
	require.NoError(t, err)
	require.NoError(t, os.Chmod(mountPoint, 0o755))

	// CRITICAL: Use vers=4.0 explicitly, never vers=4
	opts := fmt.Sprintf("vers=4.0,port=%d,sec=%s,actimeo=0", port, secFlavor)

	// Retry mount a few times (Kerberos setup may need time)
	var lastErr error
	for i := 0; i < 5; i++ {
		cmd := exec.Command("mount", "-t", "nfs", "-o", opts,
			fmt.Sprintf("localhost:%s", export), mountPoint)
		output, err := cmd.CombinedOutput()
		if err == nil {
			m := &krbV4Mount{
				t:         t,
				path:      mountPoint,
				port:      port,
				secFlavor: secFlavor,
			}
			t.Cleanup(m.Cleanup)
			return m
		}
		lastErr = fmt.Errorf("mount failed: %s: %w", string(output), err)
		time.Sleep(time.Second)
	}

	require.NoError(t, lastErr, "failed to mount NFS with Kerberos (vers=4.0, sec=%s) after retries", secFlavor)
	return nil
}

// =============================================================================
// Test: NFSv4 Kerberos Extended (vers=4.0 explicit)
// =============================================================================

// TestNFSv4KerberosExtended runs extended Kerberos authentication tests
// specifically using vers=4.0 (NOT vers=4). This is the PRIMARY test for
// locked decision #5: all three Kerberos flavors must work with explicit
// vers=4.0 mounts.
//
// Subtests:
//   - AuthorizationDenial: unmapped user gets EACCES/EPERM
//   - FileOwnershipMapping: alice creates file, stat shows uid=1001
//   - MultiFlavorV4: krb5/krb5i/krb5p all work with vers=4.0
//   - KerberosWithAuthSysFallback: sec=sys works on AUTH_SYS share
//   - ConcurrentKerberosV4Users: two users, same share, cross-visibility
func TestNFSv4KerberosExtended(t *testing.T) {
	if os.Getenv("DITTOFS_E2E_SKIP_KERBEROS") == "1" {
		t.Skip("Kerberos tests skipped via DITTOFS_E2E_SKIP_KERBEROS=1")
	}

	framework.SkipIfDarwin(t)

	// Check for required Kerberos tools
	checkKerberosV4Prereqs(t)

	// Start KDC container
	kdc := framework.NewKDCHelper(t, framework.KDCConfig{
		Realm: "DITTOFS.LOCAL",
	})

	// Add test user principals
	kdc.AddPrincipal(t, "alice", "alice123")
	kdc.AddPrincipal(t, "bob", "bob123")
	kdc.AddPrincipal(t, "unauthorized_user", "unauth123")

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

	configPath := createKerberosV4Config(t, kdc, nfsPort, apiPort)

	// Start server
	sp := helpers.StartServerProcessWithConfig(t, configPath)
	t.Cleanup(sp.ForceKill)

	// Login and create shares
	runner := helpers.LoginAsAdmin(t, sp.APIURL())

	// /krb-v4: Kerberos-protected share. Principals the static map does not
	// name resolve to the default uid, which has no DittoFS user and so gets
	// only the share's "read" default; alice and bob are granted read-write.
	// The mount itself runs on the client's machine credential, which also
	// resolves to the default uid, so "read" is what lets it traverse the root.
	setupKerberosV4Share(t, runner, "/krb-v4", helpers.WithShareDefaultPermission("read"))
	for _, u := range []krbV4User{krbAlice, krbBob} {
		_, err = runner.CreateUser(u.principal, u.password, helpers.WithUID(u.uid))
		require.NoError(t, err, "create user %s", u.principal)
		require.NoError(t, runner.GrantUserPermission("/krb-v4", u.principal, "read-write"),
			"grant read-write to %s", u.principal)
	}
	// /auth-sys-v4: allows AUTH_SYS (for fallback test)
	setupKerberosV4Share(t, runner, "/auth-sys-v4", helpers.WithShareDefaultPermission("read-write"))

	// Enable NFS adapter
	_, err = runner.EnableAdapter("nfs", helpers.WithAdapterPort(nfsPort))
	if err != nil {
		logContent, _ := os.ReadFile(sp.LogFile())
		t.Logf("Server logs:\n%s", string(logContent))
		require.NoError(t, err)
	}
	err = helpers.WaitForAdapterStatus(t, runner, "nfs", true, 30*time.Second)
	require.NoError(t, err)

	// Wait for NFS to be ready
	framework.WaitForServer(t, nfsPort, 30*time.Second)

	// Install system keytab for rpc.gssd
	installSystemKeytabV4(t, kdc)

	// --- Subtests ---

	t.Run("AuthorizationDenial", func(t *testing.T) {
		// unauthorized_user is absent from the static map, so the server
		// resolves it to the default uid, which holds only the "read" default.
		kinitAsLocalUID(t, kdc, krbAlice)
		kinitAsLocalUID(t, kdc, krbUnauthorized)

		mount := mountNFSv40WithKerberos(t, nfsPort, "/krb-v4", "krb5")

		// alice's grant opens the share, so the denial below is about the
		// principal and not about a share nobody can write.
		controlFile := mount.FilePath("authz_control.txt")
		require.NoError(t, framework.WriteFileAsUID(t, krbAlice.uid, krbAlice.uid, controlFile, []byte("ok")),
			"alice (granted read-write) should write")

		// A read that succeeds proves unauthorized_user holds a working GSS
		// context; a missing ticket would also surface as "permission denied".
		got, err := framework.ReadFileAsUID(t, krbUnauthorized.uid, krbUnauthorized.uid, controlFile)
		require.NoError(t, err, "unauthorized_user should read under the share's read default")
		assert.Equal(t, "ok", string(got))

		err = framework.WriteFileAsUID(t, krbUnauthorized.uid, krbUnauthorized.uid,
			mount.FilePath("unauthorized_test.txt"), []byte("should fail"))
		requireAccessDenied(t, err, "unmapped principal write")
	})

	t.Run("FileOwnershipMapping", func(t *testing.T) {
		kinitAsLocalUID(t, kdc, krbAlice)

		mount := mountNFSv40WithKerberos(t, nfsPort, "/krb-v4", "krb5")

		testFile := mount.FilePath("alice_ownership.txt")
		require.NoError(t, framework.WriteFileAsUID(t, krbAlice.uid, krbAlice.uid, testFile, []byte("Alice's file")),
			"alice should be able to create a file")

		// The static map resolves alice@REALM to uid 1001; the file the server
		// created must carry that owner.
		info, err := os.Stat(testFile)
		require.NoError(t, err, "should stat alice's file")
		st, ok := info.Sys().(*syscall.Stat_t)
		require.True(t, ok, "stat should expose syscall.Stat_t")
		assert.Equal(t, krbAlice.uid, st.Uid, "file should be owned by alice's mapped uid")
	})

	t.Run("MultiFlavorV4", func(t *testing.T) {
		// This is the PRIMARY test for locked decision #5:
		// All three Kerberos flavors must work with vers=4.0 explicitly.
		flavors := []string{"krb5", "krb5i", "krb5p"}

		for _, flavor := range flavors {
			flavor := flavor
			t.Run(flavor, func(t *testing.T) {
				kinitAsLocalUID(t, kdc, krbAlice)

				// Mount with this flavor and vers=4.0
				mount := mountNFSv40WithKerberos(t, nfsPort, "/krb-v4", flavor)

				// Create file
				testFile := mount.FilePath(fmt.Sprintf("multi_flavor_%s.txt", flavor))
				testData := fmt.Sprintf("Data protected by %s with vers=4.0", flavor)
				err := framework.WriteFileAsUID(t, krbAlice.uid, krbAlice.uid, testFile, []byte(testData))
				require.NoError(t, err, "%s: should create file", flavor)

				// Read back and verify
				content, err := framework.ReadFileAsUID(t, krbAlice.uid, krbAlice.uid, testFile)
				require.NoError(t, err, "%s: should read file", flavor)
				assert.Equal(t, testData, string(content),
					"%s: content should round-trip correctly", flavor)

				t.Logf("MultiFlavorV4: %s with vers=4.0 passed", flavor)
			})
		}
	})

	t.Run("KerberosWithAuthSysFallback", func(t *testing.T) {
		// Mount /auth-sys-v4 with sec=sys and vers=4.0
		// This should work since the share allows AUTH_SYS
		mountPoint := t.TempDir()

		// CRITICAL: Use vers=4.0 explicitly
		opts := fmt.Sprintf("vers=4.0,port=%d,sec=sys,actimeo=0", nfsPort)

		var lastErr error
		var mounted bool
		for i := 0; i < 3; i++ {
			cmd := exec.Command("mount", "-t", "nfs", "-o", opts,
				"localhost:/auth-sys-v4", mountPoint)
			output, err := cmd.CombinedOutput()
			if err == nil {
				mounted = true
				break
			}
			lastErr = fmt.Errorf("mount failed: %s: %w", string(output), err)
			time.Sleep(time.Second)
		}

		if !mounted {
			t.Skipf("AUTH_SYS fallback mount not supported: %v", lastErr)
		}

		defer func() {
			unmountCmd := exec.Command("umount", mountPoint)
			_ = unmountCmd.Run()
		}()

		// Create file via AUTH_SYS
		testFile := filepath.Join(mountPoint, "authsys_v4_test.txt")
		err := os.WriteFile(testFile, []byte("AUTH_SYS via vers=4.0"), 0644)
		require.NoError(t, err, "Should create file via AUTH_SYS on vers=4.0 mount")

		// Read back
		content, err := os.ReadFile(testFile)
		require.NoError(t, err, "Should read file via AUTH_SYS on vers=4.0 mount")
		assert.Equal(t, "AUTH_SYS via vers=4.0", string(content))

		t.Log("AUTH_SYS fallback on vers=4.0 mount passed")
	})

	t.Run("ConcurrentKerberosV4Users", func(t *testing.T) {
		// alice and bob act through one mount, each on their own GSS context.
		kinitAsLocalUID(t, kdc, krbAlice)
		kinitAsLocalUID(t, kdc, krbBob)

		mount := mountNFSv40WithKerberos(t, nfsPort, "/krb-v4", "krb5")

		aliceFile := mount.FilePath("concurrent_v4_alice.txt")
		require.NoError(t, framework.WriteFileAsUID(t, krbAlice.uid, krbAlice.uid, aliceFile, []byte("From Alice (vers=4.0)")),
			"alice should create file")

		bobFile := mount.FilePath("concurrent_v4_bob.txt")
		require.NoError(t, framework.WriteFileAsUID(t, krbBob.uid, krbBob.uid, bobFile, []byte("From Bob (vers=4.0)")),
			"bob should create file")

		got, err := framework.ReadFileAsUID(t, krbAlice.uid, krbAlice.uid, bobFile)
		require.NoError(t, err, "alice should read bob's file")
		assert.Equal(t, "From Bob (vers=4.0)", string(got))

		got, err = framework.ReadFileAsUID(t, krbBob.uid, krbBob.uid, aliceFile)
		require.NoError(t, err, "bob should read alice's file")
		assert.Equal(t, "From Alice (vers=4.0)", string(got))
	})
}

// =============================================================================
// Helper Functions (v4.0 specific)
// =============================================================================

// checkKerberosV4Prereqs verifies that Kerberos tools are available.
func checkKerberosV4Prereqs(t *testing.T) {
	t.Helper()

	if _, err := exec.LookPath("kinit"); err != nil {
		t.Skip("kinit not found - install krb5-user package")
	}

	if _, err := exec.LookPath("mount.nfs"); err != nil {
		t.Skip("mount.nfs not found - install nfs-common package")
	}
}

// createKerberosV4Config creates a server config with Kerberos enabled for v4.0 tests.
func createKerberosV4Config(t *testing.T, kdc *framework.KDCHelper, nfsPort, apiPort int) string {
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
    secret: "kerberos-v4-e2e-test-jwt-secret-minimum-32-characters"

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

	// Set environment for admin password
	t.Setenv("DITTOFS_ADMIN_INITIAL_PASSWORD", "adminpassword")

	return configPath
}

// setupKerberosV4Share creates a memory/memory share for Kerberos v4 tests.
func setupKerberosV4Share(t *testing.T, runner *helpers.CLIRunner, shareName string, opts ...helpers.ShareOption) {
	t.Helper()

	metaStore := fmt.Sprintf("meta-%s", strings.TrimPrefix(shareName, "/"))
	blockStore := fmt.Sprintf("local-%s", strings.TrimPrefix(shareName, "/"))

	_, err := runner.CreateInMemoryMetadataStore(metaStore)
	require.NoError(t, err)
	_, err = runner.CreateBlockStore(blockStore, "memory")
	require.NoError(t, err)
	_, err = runner.CreateShare(shareName, metaStore, blockStore, opts...)
	require.NoError(t, err)
}

// installSystemKeytabV4 installs the test keytab and krb5.conf to system locations
// for rpc.gssd to use with vers=4.0 mounts.
func installSystemKeytabV4(t *testing.T, kdc *framework.KDCHelper) {
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
		_ = exec.Command("systemctl", "restart", "rpc-gssd").Run()
	})

	// Install test krb5.conf to system location
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

	// Wait for rpc-gssd to restart
	time.Sleep(500 * time.Millisecond)

	// Get machine credentials
	hostname, _ := os.Hostname()
	kdc.KinitWithKeytab(t, fmt.Sprintf("nfs/%s", hostname), kdc.KeytabPath())
}

// krbV4User is a test principal paired with the local uid whose processes act
// as it. The uid doubles as the gid.
type krbV4User struct {
	principal string
	password  string
	uid       uint32
}

var (
	krbAlice = krbV4User{"alice", "alice123", 1001}
	krbBob   = krbV4User{"bob", "bob123", 1002}
	// No static-map entry: the server resolves it to the default uid.
	krbUnauthorized = krbV4User{"unauthorized_user", "unauth123", 4002}
)

// kinitAsLocalUID gets u a ticket in the credential cache rpc.gssd consults for
// u.uid. rpc.gssd serves root from the machine keytab and every other uid from
// a FILE cache named krb5cc_<uid> under /tmp that the uid owns, so only file
// operations run as u.uid (framework.*AsUID) act as u's principal.
func kinitAsLocalUID(t *testing.T, kdc *framework.KDCHelper, u krbV4User) {
	t.Helper()

	ccPath := fmt.Sprintf("/tmp/krb5cc_%d", u.uid)
	cmd := exec.Command("kinit", "-c", ccPath, fmt.Sprintf("%s@%s", u.principal, kdc.Realm()))
	cmd.Env = append(os.Environ(), "KRB5_CONFIG="+kdc.Krb5ConfigPath())
	cmd.Stdin = strings.NewReader(u.password + "\n")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "kinit %s failed: %s", u.principal, string(output))
	t.Cleanup(func() { _ = os.Remove(ccPath) })

	require.NoError(t, os.Chown(ccPath, int(u.uid), int(u.uid)), "chown credential cache for uid %d", u.uid)
}
