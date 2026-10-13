//go:build e2e

package framework

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// KDCHelper manages a MIT Kerberos KDC testcontainer for e2e tests.
type KDCHelper struct {
	container  testcontainers.Container
	realm      string
	keytabDir  string
	ccacheDir  string // Directory for credential caches
	krb5Config string
	kdcHost    string
	kdcPort    int
}

// KDCConfig holds configuration for the KDC container.
type KDCConfig struct {
	Realm string
}

// NewKDCHelper creates and starts a MIT Kerberos KDC container.
func NewKDCHelper(t *testing.T, cfg KDCConfig) *KDCHelper {
	t.Helper()

	if cfg.Realm == "" {
		cfg.Realm = "DITTOFS.LOCAL"
	}

	ctx := context.Background()

	// Create temp directory for keytabs
	keytabDir := t.TempDir()

	// Build the KDC container from the integration test Dockerfile
	dockerfilePath := filepath.Join(getProjectRoot(t), "test", "integration", "kerberos")

	req := testcontainers.ContainerRequest{
		FromDockerfile: testcontainers.FromDockerfile{
			Context:    dockerfilePath,
			Dockerfile: "Dockerfile",
		},
		ExposedPorts: []string{"88/tcp", "88/udp", "749/tcp"},
		Env: map[string]string{
			"KRB5_REALM": cfg.Realm,
			"KRB5_KDC":   "localhost",
		},
		Mounts: testcontainers.Mounts(
			testcontainers.BindMount(keytabDir, "/keytabs"),
		),
		WaitingFor: wait.ForListeningPort("88/tcp").WithStartupTimeout(60 * time.Second),
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err, "failed to start KDC container")

	// Get the mapped port
	mappedPort, err := container.MappedPort(ctx, "88/tcp")
	require.NoError(t, err, "failed to get KDC port")

	host, err := container.Host(ctx)
	require.NoError(t, err, "failed to get KDC host")

	// Create credential cache directory
	ccacheDir := t.TempDir()

	helper := &KDCHelper{
		container: container,
		realm:     cfg.Realm,
		keytabDir: keytabDir,
		ccacheDir: ccacheDir,
		kdcHost:   host,
		kdcPort:   int(mappedPort.Num()),
	}

	// Generate krb5.conf for this KDC instance
	helper.krb5Config = helper.generateKrb5Conf(t)

	// Set environment for Kerberos tools
	t.Setenv("KRB5_CONFIG", helper.krb5Config)
	// Set default credential cache location
	t.Setenv("KRB5CCNAME", filepath.Join(ccacheDir, "krb5cc_default"))

	t.Cleanup(func() {
		_ = container.Terminate(context.Background())
	})

	return helper
}

// generateKrb5Conf creates a krb5.conf file for the test KDC.
func (k *KDCHelper) generateKrb5Conf(t *testing.T) string {
	t.Helper()

	content := fmt.Sprintf(`[libdefaults]
    default_realm = %s
    dns_lookup_realm = false
    dns_lookup_kdc = false
    ticket_lifetime = 24h
    renew_lifetime = 7d
    forwardable = true
    rdns = false

[realms]
    %s = {
        kdc = %s:%d
    }

[domain_realm]
    .local = %s
    localhost = %s
    127.0.0.1 = %s
`, k.realm, k.realm, k.kdcHost, k.kdcPort,
		k.realm, k.realm, k.realm)

	confPath := filepath.Join(t.TempDir(), "krb5.conf")
	err := os.WriteFile(confPath, []byte(content), 0644)
	require.NoError(t, err, "failed to write krb5.conf")

	return confPath
}

// KeytabPath returns the path to the NFS service keytab.
func (k *KDCHelper) KeytabPath() string {
	return filepath.Join(k.keytabDir, "nfs.keytab")
}

// Krb5ConfigPath returns the path to the generated krb5.conf.
func (k *KDCHelper) Krb5ConfigPath() string {
	return k.krb5Config
}

// Realm returns the Kerberos realm name.
func (k *KDCHelper) Realm() string {
	return k.realm
}

// AddPrincipal creates a new principal in the KDC.
func (k *KDCHelper) AddPrincipal(t *testing.T, principal, password string) {
	t.Helper()

	ctx := context.Background()
	cmd := fmt.Sprintf("kadmin.local -q \"addprinc -pw %s %s@%s\"", password, principal, k.realm)

	exitCode, _, err := k.container.Exec(ctx, []string{"bash", "-c", cmd})
	require.NoError(t, err, "failed to exec kadmin.local")
	require.Equal(t, 0, exitCode, "kadmin.local failed")
}

// AddServicePrincipal creates a service principal and exports it to the keytab.
func (k *KDCHelper) AddServicePrincipal(t *testing.T, service, hostname string) {
	t.Helper()

	ctx := context.Background()
	principal := fmt.Sprintf("%s/%s@%s", service, hostname, k.realm)

	// Create principal with random key
	cmd := fmt.Sprintf("kadmin.local -q \"addprinc -randkey %s\"", principal)
	exitCode, _, err := k.container.Exec(ctx, []string{"bash", "-c", cmd})
	require.NoError(t, err, "failed to create service principal")
	require.Equal(t, 0, exitCode, "kadmin.local addprinc failed")

	// Export to keytab
	cmd = fmt.Sprintf("kadmin.local -q \"ktadd -k /keytabs/nfs.keytab %s\"", principal)
	exitCode, _, err = k.container.Exec(ctx, []string{"bash", "-c", cmd})
	require.NoError(t, err, "failed to export keytab")
	require.Equal(t, 0, exitCode, "kadmin.local ktadd failed")
}

// Kinit obtains a Kerberos ticket for the given principal.
func (k *KDCHelper) Kinit(t *testing.T, principal, password string) {
	t.Helper()
	k.kinit(t, principal, password, filepath.Join(k.ccacheDir, "krb5cc_default"))
}

// KinitForCIFSMount obtains a ticket like Kinit, and also stores it where the
// kernel CIFS client's key upcall finds it when this process mounts.
//
// A sec=krb5 mount gets its service ticket from cifs.upcall. For credential
// uid 0, cifs.upcall never reads KRB5CCNAME from the mounting process
// (cifs-utils cifs.upcall.c: "We can't reasonably do this for root"), so a
// root mount only finds a ticket in root's default cache, FILE:/tmp/krb5cc_0
// (the test krb5.conf sets no default_ccache_name). With the ticket only in
// the per-test cache, the mount fails with ENOKEY, and mount.cifs's retry with
// cruid=$SUDO_UID cannot open the root-owned per-test cache either.
//
// Whatever was at that path before is restored when the test ends.
func (k *KDCHelper) KinitForCIFSMount(t *testing.T, principal, password string) {
	t.Helper()
	k.Kinit(t, principal, password)

	upcallCache := fmt.Sprintf("/tmp/krb5cc_%d", os.Getuid())
	orig, readErr := os.ReadFile(upcallCache)
	if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
		require.NoError(t, readErr, "read %s before replacing it", upcallCache)
	}
	t.Cleanup(func() {
		if readErr == nil {
			_ = os.WriteFile(upcallCache, orig, 0600)
		} else {
			_ = os.Remove(upcallCache)
		}
	})
	k.kinit(t, principal, password, upcallCache)
}

func (k *KDCHelper) kinit(t *testing.T, principal, password, ccachePath string) {
	t.Helper()

	fullPrincipal := principal
	if !strings.Contains(principal, "@") {
		fullPrincipal = fmt.Sprintf("%s@%s", principal, k.realm)
	}

	cmd := exec.Command("kinit", "-c", ccachePath, fullPrincipal)
	cmd.Env = append(os.Environ(),
		"KRB5_CONFIG="+k.krb5Config,
		"KRB5CCNAME="+ccachePath,
	)
	cmd.Stdin = strings.NewReader(password + "\n")

	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "kinit failed: %s", string(output))
}

// RestartGSSD restarts rpc.gssd so it picks up the krb5.conf and keytab a
// test has just installed. The unit only starts once /etc/krb5.keytab exists
// (ConditionPathExists), so on a host without a keytab it isn't running until
// a test installs one and restarts it. Without it, a sec=krb5 NFS mount fails
// with "an incorrect mount option was specified".
func RestartGSSD(t *testing.T) {
	t.Helper()
	if err := exec.Command("systemctl", "restart", "rpc-gssd").Run(); err != nil {
		t.Logf("Warning: could not restart rpc-gssd: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
}

// InstallSystemKerberos puts the test KDC's krb5.conf and keytab at
// /etc/krb5.conf and /etc/krb5.keytab, where rpc.gssd and cifs.upcall read
// them, and restarts rpc.gssd onto them. When the test ends, each file goes
// back to what the host had: its old content, or no file at all when there
// was none, so a host without a keytab doesn't keep the test realm's keys and
// start rpc.gssd on them. rpc.gssd is then restarted again.
//
// The restore is registered before anything is written, so a failed install
// is undone too. A file that exists but can't be read is left alone, since it
// couldn't be restored.
func (k *KDCHelper) InstallSystemKerberos(t *testing.T) error {
	t.Helper()

	// Registered first so it runs last, after the files are back.
	t.Cleanup(func() { RestartGSSD(t) })
	if err := installFiles(t, []installedFile{
		{path: "/etc/krb5.conf", src: k.Krb5ConfigPath(), perm: 0644},
		{path: "/etc/krb5.keytab", src: k.KeytabPath(), perm: 0600},
	}); err != nil {
		return err
	}
	RestartGSSD(t)
	return nil
}

// installedFile is a file a test puts at path from src, with perm when it
// creates it.
type installedFile struct {
	path string
	src  string
	perm fs.FileMode
}

// installFiles copies each file's src to its path and, when the test ends,
// puts back what was at each path before, or removes the file if there was
// none. InstallSystemKerberos is its only caller; it is separate so the restore
// can be tested on paths a test owns.
func installFiles(t *testing.T, files []installedFile) error {
	t.Helper()

	type saved struct {
		installedFile
		data    []byte
		existed bool
	}
	originals := make([]saved, 0, len(files))
	for _, f := range files {
		data, err := os.ReadFile(f.path)
		switch {
		case err == nil:
			originals = append(originals, saved{f, data, true})
		case errors.Is(err, fs.ErrNotExist):
			originals = append(originals, saved{installedFile: f})
		default:
			return fmt.Errorf("read %s before replacing it: %w", f.path, err)
		}
	}

	t.Cleanup(func() {
		for _, o := range originals {
			if o.existed {
				_ = os.WriteFile(o.path, o.data, o.perm)
			} else {
				_ = os.Remove(o.path)
			}
		}
	})

	for _, f := range files {
		data, err := os.ReadFile(f.src)
		if err != nil {
			return fmt.Errorf("read %s: %w", f.src, err)
		}
		if err := os.WriteFile(f.path, data, f.perm); err != nil {
			return fmt.Errorf("install %s (needs root): %w", f.path, err)
		}
	}
	return nil
}

// SkipOrFailKerberosMount skips a test whose Kerberos mount failed. With
// DITTOFS_E2E_REQUIRE_KERBEROS=1 it fails the test instead, so a broken host
// setup or a server-side Kerberos failure can't pass as a skip.
func SkipOrFailKerberosMount(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("DITTOFS_E2E_REQUIRE_KERBEROS") == "1" {
		t.Fatalf("Kerberos mount required (DITTOFS_E2E_REQUIRE_KERBEROS=1): "+format, args...)
	}
	t.Skipf(format, args...)
}

// KinitWithKeytab obtains a Kerberos ticket using a keytab.
func (k *KDCHelper) KinitWithKeytab(t *testing.T, principal, keytabPath string) {
	t.Helper()

	fullPrincipal := principal
	if !strings.Contains(principal, "@") {
		fullPrincipal = fmt.Sprintf("%s@%s", principal, k.realm)
	}

	ccachePath := filepath.Join(k.ccacheDir, "krb5cc_default")
	cmd := exec.Command("kinit", "-k", "-t", keytabPath, "-c", ccachePath, fullPrincipal)
	cmd.Env = append(os.Environ(),
		"KRB5_CONFIG="+k.krb5Config,
		"KRB5CCNAME="+ccachePath,
	)

	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "kinit with keytab failed: %s", string(output))
}

// Kdestroy destroys the current Kerberos ticket cache.
func (k *KDCHelper) Kdestroy(t *testing.T) {
	t.Helper()

	ccachePath := filepath.Join(k.ccacheDir, "krb5cc_default")
	cmd := exec.Command("kdestroy", "-c", ccachePath)
	cmd.Env = append(os.Environ(),
		"KRB5_CONFIG="+k.krb5Config,
		"KRB5CCNAME="+ccachePath,
	)
	_ = cmd.Run() // Ignore errors - may not have a ticket
}

// Klist lists the current Kerberos tickets.
func (k *KDCHelper) Klist(t *testing.T) string {
	t.Helper()

	ccachePath := filepath.Join(k.ccacheDir, "krb5cc_default")
	cmd := exec.Command("klist", "-c", ccachePath)
	cmd.Env = append(os.Environ(),
		"KRB5_CONFIG="+k.krb5Config,
		"KRB5CCNAME="+ccachePath,
	)
	output, _ := cmd.CombinedOutput()
	return string(output)
}

// CCachePath returns the path to the default credential cache.
func (k *KDCHelper) CCachePath() string {
	return filepath.Join(k.ccacheDir, "krb5cc_default")
}

// getProjectRoot returns the project root directory.
func getProjectRoot(t *testing.T) string {
	t.Helper()

	// Walk up from current directory to find go.mod
	dir, err := os.Getwd()
	require.NoError(t, err)

	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find project root (go.mod)")
		}
		dir = parent
	}
}

// KerberosMount extends the Mount type with Kerberos-specific functionality.
type KerberosMount struct {
	*Mount
	secFlavor string
}

// MountNFSWithKerberos mounts an NFS share with Kerberos authentication.
// secFlavor should be "krb5", "krb5i", or "krb5p".
func MountNFSWithKerberos(t *testing.T, port int, export, secFlavor string, nfsVersion int) *KerberosMount {
	t.Helper()

	if export == "" {
		export = "/export"
	}

	mountPoint := t.TempDir()

	var opts string
	if nfsVersion == 4 {
		opts = fmt.Sprintf("vers=4,port=%d,sec=%s,actimeo=0", port, secFlavor)
	} else {
		opts = fmt.Sprintf("nfsvers=3,tcp,port=%d,mountport=%d,sec=%s,actimeo=0,nolock",
			port, port, secFlavor)
	}

	// Retry mount a few times (Kerberos setup may need time)
	var lastErr error
	for i := 0; i < 5; i++ {
		cmd := exec.Command("mount", "-t", "nfs", "-o", opts,
			fmt.Sprintf("localhost:%s", export), mountPoint)
		output, err := cmd.CombinedOutput()
		if err == nil {
			m := &Mount{
				T:        t,
				Path:     mountPoint,
				Protocol: "nfs",
				Port:     port,
				mounted:  true,
			}
			t.Cleanup(func() {
				m.Cleanup()
			})
			return &KerberosMount{
				Mount:     m,
				secFlavor: secFlavor,
			}
		}
		lastErr = fmt.Errorf("mount failed: %s: %w", string(output), err)
		time.Sleep(time.Second)
	}

	require.NoError(t, lastErr, "failed to mount NFS with Kerberos after retries")
	return nil
}

// SecFlavor returns the security flavor used for this mount.
func (m *KerberosMount) SecFlavor() string {
	return m.secFlavor
}

// MountNFSWithKerberosAndError mounts an NFS share with Kerberos, returning error instead of failing.
func MountNFSWithKerberosAndError(t *testing.T, port int, export, secFlavor string, nfsVersion int) (*KerberosMount, error) {
	t.Helper()

	if export == "" {
		export = "/export"
	}

	mountPoint := t.TempDir()

	var opts string
	if nfsVersion == 4 {
		opts = fmt.Sprintf("vers=4,port=%d,sec=%s,actimeo=0", port, secFlavor)
	} else {
		opts = fmt.Sprintf("nfsvers=3,tcp,port=%d,mountport=%d,sec=%s,actimeo=0,nolock",
			port, port, secFlavor)
	}

	// Retry mount a few times
	var lastErr error
	for i := 0; i < 3; i++ {
		cmd := exec.Command("mount", "-t", "nfs", "-o", opts,
			fmt.Sprintf("localhost:%s", export), mountPoint)
		output, err := cmd.CombinedOutput()
		if err == nil {
			m := &Mount{
				T:        t,
				Path:     mountPoint,
				Protocol: "nfs",
				Port:     port,
				mounted:  true,
			}
			return &KerberosMount{
				Mount:     m,
				secFlavor: secFlavor,
			}, nil
		}
		lastErr = fmt.Errorf("mount failed: %s: %w", string(output), err)
		time.Sleep(time.Second)
	}

	return nil, lastErr
}
