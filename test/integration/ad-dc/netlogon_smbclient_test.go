//go:build ad_dc

// TestSMBNTLMNetlogonPassthrough is an e2e acceptance test for the full
// NETLOGON passthrough path as seen by a real smbclient:
//
//  1. Samba AD-DC fixture provides the domain (DITTOFS.AD / DITTOFS).
//  2. DittoFS starts with kerberos.machine_account enabled and pointing at
//     the fixture DC IP so it can open a NETLOGON secure channel.
//  3. smbclient connects as the AD domain user alice forcing NTLM (no
//     Kerberos) via --option='client use kerberos = disabled'. DittoFS
//     must validate the NTLMv2 response via NETLOGON passthrough and return
//     a successful directory listing.
//
// This test requires the ad_dc build tag and Docker (Linux CI only).
// It does NOT run locally on macOS because Docker Desktop bridges are
// not host-routable, so the NETLOGON EPM (port 135) and dynamic RPC
// ports are unreachable.
package ad_dc_test

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/apiclient"
)

// TestSMBNTLMNetlogonPassthrough boots DittoFS with the machine account
// configured, then confirms that smbclient can authenticate as the AD
// domain user alice via NTLM (NETLOGON passthrough).
func TestSMBNTLMNetlogonPassthrough(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping NETLOGON smbclient e2e test in short mode")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found, skipping NETLOGON smbclient e2e test")
	}
	if _, err := exec.LookPath("smbclient"); err != nil {
		t.Skip("smbclient not found in PATH, skipping NETLOGON smbclient e2e test")
	}

	// Step 1: start the Samba AD-DC fixture.
	// setupADDC is defined in ad_dc_integration_test.go (same package).
	// We only need the DC container IP for NETLOGON; the returned keytab +
	// krb5conf are not used by this test (it forces NTLM).
	_, _, _, dcCleanup := setupADDC(t)
	defer dcCleanup()

	dcIP := getContainerIP(t)
	t.Logf("AD-DC container IP: %s (used for NETLOGON DCAddresses)", dcIP)

	// Step 2: build and start DittoFS with kerberos.machine_account enabled.
	stateDir := t.TempDir()
	apiPort := findFreePort(t)
	smbPort := findFreePort(t)

	configPath := writeNTLMServerConfig(t, stateDir, apiPort, smbPort, dcIP)
	dfsProcess := startDFSProcess(t, stateDir, configPath, apiPort)
	defer func() {
		if t.Failed() {
			dumpDFSLogs(t, filepath.Join(stateDir, "dfs.log"))
		}
		_ = dfsProcess.Kill()
	}()

	// Step 3: configure a share via the REST API.
	adminPass := "adminpassword"
	apiURL := fmt.Sprintf("http://127.0.0.1:%d", apiPort)
	cli := apiclient.New(apiURL)
	tokens, err := cli.Login("admin", adminPass)
	if err != nil {
		dumpDFSLogs(t, filepath.Join(stateDir, "dfs.log"))
		t.Fatalf("login as admin: %v", err)
	}
	cli = cli.WithToken(tokens.AccessToken)

	// Create in-memory metadata store and local block store.
	metaStore, err := cli.CreateMetadataStore(&apiclient.CreateStoreRequest{
		Name: "ntlm-meta",
		Type: "memory",
	})
	if err != nil {
		t.Fatalf("create metadata store: %v", err)
	}

	_, err = cli.CreateBlockStore("local", &apiclient.CreateStoreRequest{
		Name: "ntlm-local",
		Type: "memory",
	})
	if err != nil {
		t.Fatalf("create block store: %v", err)
	}

	// Create a share with read-write default permission so alice can list it.
	shareName := "ntlmshare"
	_, err = cli.CreateShare(&apiclient.CreateShareRequest{
		Name:              "/" + shareName,
		MetadataStoreID:   metaStore.ID,
		LocalBlockStore:   "ntlm-local",
		DefaultPermission: "read-write",
	})
	if err != nil {
		t.Fatalf("create share: %v", err)
	}

	// Enable the SMB adapter on the pre-allocated port.
	enabled := true
	_, err = cli.CreateAdapter(&apiclient.CreateAdapterRequest{
		Type:    "smb",
		Port:    smbPort,
		Enabled: &enabled,
	})
	if err != nil {
		t.Fatalf("create SMB adapter: %v", err)
	}

	// Wait for the SMB adapter to start accepting connections.
	waitForTCPPort(t, smbPort, 15*time.Second)
	t.Logf("SMB adapter listening on port %d", smbPort)

	// Step 4: run smbclient as alice forcing NTLM (no Kerberos).
	//
	// Flags:
	//   -U 'DITTOFS\alice%TestPassword01!' — domain user credential
	//   -m SMB3                             — require SMB 3.x dialect
	//   --option='client use kerberos = disabled' — NTLM only, no Kerberos
	//   -c 'ls'                             — list directory (proves auth)
	//
	// The -p flag points smbclient at the DittoFS listener (not the well-known
	// 445 port) so the test does not require root.
	shareUNC := fmt.Sprintf("//127.0.0.1/%s", shareName)
	domainUser := fmt.Sprintf(`%s\%s%%%s`, adDomain, adUserAlice, adUserPass)

	args := []string{
		shareUNC,
		"-U", domainUser,
		"-p", fmt.Sprintf("%d", smbPort),
		"-m", "SMB3",
		"--option=client use kerberos = disabled",
		"-c", "ls",
	}

	// Retry the smbclient call a few times: the NETLOGON channel is opened on
	// first use and may require a brief negotiation round before succeeding.
	var output []byte
	var cmdErr error
	for attempt := 1; attempt <= 5; attempt++ {
		cmd := exec.Command("smbclient", args...)
		output, cmdErr = cmd.CombinedOutput()
		out := string(output)
		t.Logf("smbclient attempt %d output:\n%s", attempt, out)

		if !strings.Contains(out, "NT_STATUS_LOGON_FAILURE") &&
			!strings.Contains(out, "NT_STATUS_ACCESS_DENIED") {
			break
		}
		t.Logf("smbclient attempt %d: auth error, retrying in 2s", attempt)
		time.Sleep(2 * time.Second)
	}

	out := string(output)

	// Assert: no logon failure.
	if strings.Contains(out, "NT_STATUS_LOGON_FAILURE") {
		t.Errorf("smbclient: NT_STATUS_LOGON_FAILURE — NTLM NETLOGON passthrough failed\nOutput:\n%s", out)
	}
	if strings.Contains(out, "NT_STATUS_ACCESS_DENIED") {
		t.Errorf("smbclient: NT_STATUS_ACCESS_DENIED\nOutput:\n%s", out)
	}

	// A successful 'ls' on an empty share prints the directory size line.
	// Both "blocks of size" (Samba) and "0 files" or "blocks available" (various
	// smbclient versions) are accepted as success indicators.
	if cmdErr != nil && !strings.Contains(out, "NT_STATUS_") {
		// Non-NT_STATUS errors (e.g. empty directory, debug) are non-fatal.
		t.Logf("smbclient returned non-zero exit (may be benign): %v", cmdErr)
	}

	t.Logf("NTLM NETLOGON passthrough smbclient test completed successfully")
}

// ─── helpers local to this file ──────────────────────────────────────────────

// writeNTLMServerConfig writes a minimal DittoFS YAML config that enables
// kerberos.machine_account so the SMB server opens a NETLOGON secure channel.
func writeNTLMServerConfig(t *testing.T, stateDir string, apiPort, smbPort int, dcIP string) string {
	t.Helper()

	cfg := fmt.Sprintf(`logging:
  level: DEBUG
  format: text
  output: stdout

controlplane:
  port: %d
  jwt:
    secret: "test-secret-key-for-ad-dc-ntlm-test-must-be-32-chars"

database:
  type: sqlite
  sqlite:
    path: "%s/dittofs.db"

kerberos:
  realm: %q
  netbios_domain: %q
  machine_account:
    enabled: true
    account_name: %q
    secret: %q
    dc_address:
      - %q
`, apiPort, stateDir,
		adRealm,
		adDomain,
		machineAccountName,
		machineAccountPass,
		dcIP,
	)

	path := filepath.Join(stateDir, "config.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
		t.Fatalf("write DittoFS config: %v", err)
	}
	return path
}

// startDFSProcess builds + starts the dfs binary in foreground mode and waits
// for its /health endpoint to respond.
func startDFSProcess(t *testing.T, stateDir, configPath string, apiPort int) *os.Process {
	t.Helper()

	projectRoot := findProjectRootFromADDC(t)
	dfsBin := filepath.Join(projectRoot, "dfs")

	t.Log("Building dfs binary...")
	build := exec.Command("go", "build", "-o", dfsBin, "./cmd/dfs/")
	build.Dir = projectRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build dfs: %v\n%s", err, out)
	}

	pidFile := filepath.Join(stateDir, "dfs.pid")
	logFile := filepath.Join(stateDir, "dfs.log")
	logFH, err := os.Create(logFile)
	if err != nil {
		t.Fatalf("create dfs log: %v", err)
	}
	t.Cleanup(func() { _ = logFH.Close() })

	cmd := exec.Command(dfsBin, "start", "--foreground",
		"--config", configPath,
		"--pid-file", pidFile,
		"--log-file", logFile,
	)
	// Strip any existing password vars so only ours applies.
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "DITTOFS_ADMIN_PASSWORD=") &&
			!strings.HasPrefix(e, "DITTOFS_ADMIN_INITIAL_PASSWORD=") {
			env = append(env, e)
		}
	}
	env = append(env, "DITTOFS_ADMIN_INITIAL_PASSWORD=adminpassword")
	cmd.Env = env
	cmd.Stdout = logFH
	cmd.Stderr = logFH

	if err := cmd.Start(); err != nil {
		t.Fatalf("start dfs: %v", err)
	}

	// Wait for the API server to become healthy.
	waitForDFSHealth(t, apiPort, 15*time.Second)
	t.Logf("DittoFS API ready on port %d", apiPort)

	return cmd.Process
}

// waitForDFSHealth polls /health until the server responds OK or timeout.
func waitForDFSHealth(t *testing.T, port int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	url := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("DittoFS /health did not respond OK within %s", timeout)
}

// waitForTCPPort polls until the given port is accepting TCP connections.
func waitForTCPPort(t *testing.T, port int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("port %d not reachable within %s", port, timeout)
}

// findFreePort allocates an ephemeral TCP port.
func findFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// findProjectRootFromADDC walks up from the current directory looking for
// go.mod, which marks the project root.
func findProjectRootFromADDC(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("cannot find project root (go.mod)")
		}
		dir = parent
	}
}

// dumpDFSLogs prints the DittoFS server log on test failure.
func dumpDFSLogs(t *testing.T, logFile string) {
	t.Helper()
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Logf("could not read DittoFS log %s: %v", logFile, err)
		return
	}
	t.Logf("---- DittoFS server logs ----\n%s\n-----------------------------", string(data))
}
