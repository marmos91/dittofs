//go:build e2e

package helpers

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	smb2 "github.com/hirochachacha/go-smb2"
	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/test/e2e/framework"
)

// SMB3TestEnv holds a complete DittoFS test environment with SMB3 adapter.
type SMB3TestEnv struct {
	ServerProcess *ServerProcess
	CLIRunner     *CLIRunner
	SMBPort       int
	ShareName     string
	Username      string
	Password      string
}

// SetupSMB3TestEnv creates a DittoFS server, stores, share, user, and SMB adapter.
// Returns a fully configured test environment ready for SMB3 testing.
// Resources are cleaned up automatically via t.Cleanup.
func SetupSMB3TestEnv(t *testing.T) *SMB3TestEnv {
	t.Helper()

	// Start server process
	sp := StartServerProcess(t, "")
	t.Cleanup(func() {
		if t.Failed() {
			sp.DumpLogs(t)
		}
		sp.ForceKill()
	})

	// Login as admin
	cli := LoginAsAdmin(t, sp.APIURL())

	// Create stores
	metaStoreName := UniqueTestName("smb3meta")
	payloadStoreName := UniqueTestName("smb3payload")

	_, err := cli.CreateMetadataStore(metaStoreName, "memory")
	require.NoError(t, err, "Should create metadata store")

	_, err = cli.CreatePayloadStore(payloadStoreName, "memory")
	require.NoError(t, err, "Should create payload store")

	// Create share with read-write default permission
	shareName := "/" + UniqueTestName("smb3share")
	_, err = cli.CreateShare(shareName, metaStoreName, payloadStoreName,
		WithShareDefaultPermission("read-write"))
	require.NoError(t, err, "Should create share")

	// Create test user (password must be 8+ characters for SMB auth)
	testUsername := UniqueTestName("smb3user")
	testPassword := "testpass123"

	_, err = cli.CreateUser(testUsername, testPassword)
	require.NoError(t, err, "Should create test user")

	// Grant user read-write permission
	err = cli.GrantUserPermission(shareName, testUsername, "read-write")
	require.NoError(t, err, "Should grant user permission")

	// Enable SMB adapter on a dynamic port
	smbPort := FindFreePort(t)
	_, err = cli.EnableAdapter("smb", WithAdapterPort(smbPort))
	require.NoError(t, err, "Should enable SMB adapter")

	// Wait for adapter to be fully enabled
	err = WaitForAdapterStatus(t, cli, "smb", true, 5*time.Second)
	require.NoError(t, err, "SMB adapter should become enabled")

	// Wait for SMB server to be listening
	framework.WaitForServer(t, smbPort, 10*time.Second)

	return &SMB3TestEnv{
		ServerProcess: sp,
		CLIRunner:     cli,
		SMBPort:       smbPort,
		ShareName:     shareName,
		Username:      testUsername,
		Password:      testPassword,
	}
}

// ConnectSMB3 creates an SMB3 session via go-smb2 using NTLM authentication.
// The session is automatically closed via t.Cleanup.
func ConnectSMB3(t *testing.T, port int, user, pass string) *smb2.Session {
	t.Helper()

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", port), 5*time.Second)
	require.NoError(t, err, "Should connect to SMB server")

	d := &smb2.Dialer{
		Initiator: &smb2.NTLMInitiator{
			User:     user,
			Password: pass,
		},
	}

	session, err := d.Dial(conn)
	if err != nil {
		_ = conn.Close()
		require.NoError(t, err, "Should establish SMB3 session")
	}

	t.Cleanup(func() {
		_ = session.Logoff()
		_ = conn.Close()
	})

	return session
}

// ConnectSMB3WithError creates an SMB3 session and returns any error instead of failing.
// Use this for negative test cases (e.g., invalid credentials).
func ConnectSMB3WithError(t *testing.T, port int, user, pass string) (*smb2.Session, error) {
	t.Helper()

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", port), 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}

	d := &smb2.Dialer{
		Initiator: &smb2.NTLMInitiator{
			User:     user,
			Password: pass,
		},
	}

	session, err := d.Dial(conn)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("dial: %w", err)
	}

	t.Cleanup(func() {
		_ = session.Logoff()
		_ = conn.Close()
	})

	return session, nil
}

// MountSMB3Share mounts an SMB3 share via a go-smb2 session.
// The share is automatically unmounted via t.Cleanup.
func MountSMB3Share(t *testing.T, session *smb2.Session, shareName string) *smb2.Share {
	t.Helper()

	// go-smb2 expects share name without leading slash
	cleanName := strings.TrimPrefix(shareName, "/")

	share, err := session.Mount(cleanName)
	require.NoError(t, err, "Should mount SMB3 share %q", cleanName)

	t.Cleanup(func() {
		_ = share.Umount()
	})

	return share
}

// RunSMBClient executes smbclient with the given command and returns output.
// Returns the combined stdout/stderr output and any error.
// If smbclient is not found, the test is skipped.
func RunSMBClient(t *testing.T, port int, user, pass, share, command string) (string, error) {
	t.Helper()

	smbclientPath, err := exec.LookPath("smbclient")
	if err != nil {
		t.Skip("smbclient not found in PATH, skipping smbclient test")
	}

	// Write credentials to a temp auth file to avoid exposing password in argv
	authFile := writeSMBAuthFile(t, user, pass)

	shareUNC := fmt.Sprintf("//localhost/%s", strings.TrimPrefix(share, "/"))

	args := []string{
		shareUNC,
		"-A", authFile,
		"-p", fmt.Sprintf("%d", port),
		"-c", command,
	}

	cmd := exec.Command(smbclientPath, args...)
	output, cmdErr := cmd.CombinedOutput()

	return string(output), cmdErr
}

// RunSMBClientDebug executes smbclient with debug output for protocol analysis.
// Returns the combined stdout/stderr output and any error.
func RunSMBClientDebug(t *testing.T, port int, user, pass, share, command string, debugLevel int) (string, error) {
	t.Helper()

	smbclientPath, err := exec.LookPath("smbclient")
	if err != nil {
		t.Skip("smbclient not found in PATH, skipping smbclient test")
	}

	authFile := writeSMBAuthFile(t, user, pass)

	shareUNC := fmt.Sprintf("//localhost/%s", strings.TrimPrefix(share, "/"))

	args := []string{
		shareUNC,
		"-A", authFile,
		"-p", fmt.Sprintf("%d", port),
		"-c", command,
		fmt.Sprintf("--debuglevel=%d", debugLevel),
		"--max-protocol=SMB3",
	}

	cmd := exec.Command(smbclientPath, args...)
	output, cmdErr := cmd.CombinedOutput()

	return string(output), cmdErr
}

// writeSMBAuthFile creates a temporary smbclient auth file with restrictive
// permissions, avoiding credential exposure in process argv.
func writeSMBAuthFile(t *testing.T, user, pass string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "smb-auth-*")
	if err != nil {
		t.Fatalf("failed to create SMB auth file: %v", err)
	}
	if err := f.Chmod(0600); err != nil {
		_ = f.Close()
		t.Fatalf("failed to set permissions on SMB auth file: %v", err)
	}
	if _, err := fmt.Fprintf(f, "username=%s\npassword=%s\n", user, pass); err != nil {
		_ = f.Close()
		t.Fatalf("failed to write SMB auth file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("failed to close SMB auth file: %v", err)
	}
	return f.Name()
}

// IsSMBClientAvailable checks if smbclient binary is available in PATH.
func IsSMBClientAvailable() bool {
	_, err := exec.LookPath("smbclient")
	return err == nil
}
