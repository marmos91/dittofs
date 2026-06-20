//go:build ad_dc

// Package ad_dc_test provides an integration test for AD-1 (#1233): decoding
// the Kerberos PAC of an Active Directory ticket and surfacing the user's
// transitive group SIDs through the shared KerberosService.AuthResult.
//
// Unlike test/integration/kerberos (an MIT KDC, auth only, no PAC), this test
// stands up a real Samba Active Directory Domain Controller in Docker. The DC
// issues service tickets carrying a Microsoft PAC (MS-PAC) whose
// GroupMembershipSIDs include the user's full transitive group set — AD
// resolves nested membership at the DC. The test logs in as alice (a member of
// devs, which is nested under engineering), obtains a service ticket for the
// SMB SPN, builds the AP-REQ, and asserts that KerberosService.Authenticate
// returns a non-empty GroupSIDs containing BOTH the devs and engineering
// domain group SIDs plus a populated UserSID.
//
// Run with: go test -tags=ad_dc -v -timeout 20m ./test/integration/ad-dc/
// Requires: Docker (the samba-ad-dc image builds + provisions under linux/amd64
// emulation on Apple Silicon, which is slow — hence the generous timeout).
package ad_dc_test

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jcmturner/gokrb5/v8/client"
	krb5config "github.com/jcmturner/gokrb5/v8/config"
	"github.com/jcmturner/gokrb5/v8/messages"
	"github.com/jcmturner/gokrb5/v8/types"

	kerbauth "github.com/marmos91/dittofs/internal/auth/kerberos"
	"github.com/marmos91/dittofs/pkg/auth/kerberos"
	dconfig "github.com/marmos91/dittofs/pkg/config"
)

const (
	adContainerName = "dittofs-ad-dc-test"
	adImageName     = "dittofs-ad-dc-test"

	adRealm  = "DITTOFS.AD"
	adDomain = "DITTOFS"

	adUserAlice = "alice"
	adUserPass  = "TestPassword01!"

	// SMB service principal the DittoFS server presents; the fixture exports
	// its key into dittofs.keytab. spnShort is the form gokrb5 uses to request
	// a service ticket (no realm); spnFull is what Authenticate validates against.
	adSMBSPNShort = "cifs/dittofs.dittofs.ad"
	adSMBSPNFull  = "cifs/dittofs.dittofs.ad@DITTOFS.AD"
)

// TestADGroupSIDsFromPAC exercises the full AD-1 path: a real AD ticket's PAC
// group SIDs flow into the KerberosService AuthResult.
func TestADGroupSIDsFromPAC(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping AD-DC integration test in short mode")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found, skipping AD-DC integration test")
	}

	// krb5ConfPath already encodes the mapped KDC port, so the gokrb5 client
	// reaches the DC without a separate port argument.
	_, keytabPath, krb5ConfPath, cleanup := setupADDC(t)
	defer cleanup()

	// Build the shared KerberosService from a provider backed by the exported
	// service keytab. This is the same construction the SMB session layer uses.
	cfg := &dconfig.KerberosConfig{
		Enabled:          true,
		KeytabPath:       keytabPath,
		ServicePrincipal: adSMBSPNFull,
		Krb5Conf:         krb5ConfPath,
		MaxClockSkew:     5 * time.Minute,
		ContextTTL:       10 * time.Minute,
		MaxContexts:      100,
	}
	provider, err := kerberos.NewProvider(cfg)
	if err != nil {
		t.Fatalf("create kerberos provider: %v", err)
	}
	defer provider.Close()

	service := kerbauth.NewKerberosService(provider)

	// alice authenticates against the DC and obtains a service ticket for the
	// SMB SPN, then we build the raw AP-REQ (Authenticate expects the raw
	// AP-REQ, not a GSS-wrapped token).
	apReqBytes := getADAPREQ(t, krb5ConfPath)

	result, err := service.Authenticate(apReqBytes, adSMBSPNFull)
	if err != nil {
		t.Fatalf("Authenticate failed: %v", err)
	}

	t.Logf("Authenticated principal=%q realm=%q user_sid=%q group_sids=%v",
		result.Principal, result.Realm, result.UserSID, result.GroupSIDs)

	// UserSID must be populated from the PAC (logon domain SID + RID).
	if result.UserSID == "" {
		t.Error("expected non-empty UserSID from PAC, got empty")
	} else if !strings.HasPrefix(result.UserSID, "S-1-5-21-") {
		t.Errorf("UserSID %q does not look like a domain SID (S-1-5-21-...)", result.UserSID)
	}

	// GroupSIDs must be non-empty and contain BOTH the devs and engineering
	// group SIDs. AD resolves the nesting (devs ⊂ engineering) at the DC and
	// stamps both into alice's ticket PAC — no LDAP group-walk required.
	if len(result.GroupSIDs) == 0 {
		t.Fatal("expected non-empty GroupSIDs from PAC, got none")
	}

	devsSID := lookupGroupSID(t, "devs")
	engineeringSID := lookupGroupSID(t, "engineering")

	if !containsSID(result.GroupSIDs, devsSID) {
		t.Errorf("GroupSIDs %v missing devs SID %q", result.GroupSIDs, devsSID)
	}
	if !containsSID(result.GroupSIDs, engineeringSID) {
		t.Errorf("GroupSIDs %v missing engineering (nested) SID %q", result.GroupSIDs, engineeringSID)
	}

	t.Logf("PAC carried devs=%s and engineering=%s (nested resolved at DC) ✓", devsSID, engineeringSID)
}

// AD-DC Docker Setup

// setupADDC builds the Samba AD-DC image, runs it (letting its own entrypoint
// provision the domain), waits for the KDC to accept connections, and copies
// the exported keytab + a host-side krb5.conf pointing at the mapped KDC port.
func setupADDC(t *testing.T) (kdcHostPort int, keytabPath, krb5ConfPath string, cleanup func()) {
	t.Helper()

	tmpDir := t.TempDir()

	// Clean up any previous container.
	_ = exec.Command("docker", "rm", "-f", adContainerName).Run()

	dockerfileDir := findADDockerfileDir(t)

	// Build the image (cached after first run). Force linux/amd64: samba-ad-dc
	// has no native arm64 path in our base and runs fine under emulation.
	t.Log("Building Samba AD-DC image (slow on first run)...")
	buildCmd := exec.Command("docker", "build",
		"--platform", "linux/amd64",
		"-t", adImageName, dockerfileDir,
	)
	buildCmd.Stdout = os.Stdout
	buildCmd.Stderr = os.Stderr
	if err := buildCmd.Run(); err != nil {
		t.Fatalf("docker build failed: %v", err)
	}

	// Run the container with the AD-DC entrypoint (it provisions the domain,
	// creates users/groups, and exports the keytab to /keytabs on its own).
	t.Log("Starting AD-DC container...")
	runOut, err := exec.Command("docker", "run", "-d",
		"--platform", "linux/amd64",
		"--name", adContainerName,
		"-p", "0:88/tcp",
		adImageName,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run failed: %v\n%s", err, runOut)
	}

	// Discover the randomly assigned host port for the KDC.
	portOut, err := exec.Command("docker", "port", adContainerName, "88/tcp").Output()
	if err != nil {
		dumpADLogs(t)
		t.Fatalf("docker port: %v", err)
	}
	portStr := strings.TrimSpace(string(portOut))
	parts := strings.Split(portStr, ":")
	kdcHostPort, err = strconv.Atoi(parts[len(parts)-1])
	if err != nil {
		t.Fatalf("parse port %q: %v", portStr, err)
	}
	t.Logf("AD-DC KDC mapped to host port %d", kdcHostPort)

	// Wait for the KDC TCP port to come up. Provisioning a fresh AD domain
	// under emulation is slow, so allow several minutes.
	t.Log("Waiting for KDC TCP port...")
	waitForADPort(t, kdcHostPort, 6*time.Minute)

	// Wait for the entrypoint to finish exporting the keytab. It is the last
	// artifact written before samba re-execs in the foreground.
	t.Log("Waiting for service keytab export...")
	waitForKeytab(t, 4*time.Minute)

	// Copy the keytab out of the container. docker cp reads as root inside the
	// container, so the fixture's chmod 0400 / chown to the dittofs uid does
	// not block us (it would block a host volume mount).
	keytabPath = filepath.Join(tmpDir, "dittofs.keytab")
	if out, err := exec.Command("docker", "cp",
		adContainerName+":/keytabs/dittofs.keytab", keytabPath).CombinedOutput(); err != nil {
		dumpADLogs(t)
		t.Fatalf("copy keytab: %v\n%s", err, out)
	}

	// Write a host-side krb5.conf that points the realm KDC at the mapped host
	// port (force TCP, the mapped port is TCP only). The Samba-generated
	// krb5.conf in the container references the DC hostname, which the host
	// cannot resolve — so we synthesise a minimal one like the MIT KDC test.
	krb5ConfPath = filepath.Join(tmpDir, "krb5.conf")
	hostKrb5Conf := fmt.Sprintf(`[libdefaults]
    default_realm = %s
    dns_lookup_realm = false
    dns_lookup_kdc = false
    udp_preference_limit = 1
    rdns = false

[realms]
    %s = {
        kdc = 127.0.0.1:%d
    }
`, adRealm, adRealm, kdcHostPort)
	if err := os.WriteFile(krb5ConfPath, []byte(hostKrb5Conf), 0o644); err != nil {
		t.Fatalf("write krb5.conf: %v", err)
	}

	cleanup = func() {
		t.Log("Cleaning up AD-DC container...")
		_ = exec.Command("docker", "rm", "-f", adContainerName).Run()
	}
	return kdcHostPort, keytabPath, krb5ConfPath, cleanup
}

func findADDockerfileDir(t *testing.T) string {
	t.Helper()
	if _, err := os.Stat("Dockerfile"); err == nil {
		return "."
	}
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, "test", "integration", "ad-dc")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("cannot find project root (go.mod)")
		}
		dir = parent
	}
}

// waitForKeytab polls until dittofs.keytab exists and is non-empty inside the
// container (the entrypoint exports it after provisioning completes).
func waitForKeytab(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := exec.Command("docker", "exec", adContainerName,
			"sh", "-c", "test -s /keytabs/dittofs.keytab && echo ok").CombinedOutput()
		if err == nil && strings.TrimSpace(string(out)) == "ok" {
			return
		}
		time.Sleep(2 * time.Second)
	}
	dumpADLogs(t)
	t.Fatalf("keytab not exported within %s", timeout)
}

func waitForADPort(t *testing.T, port int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 1*time.Second)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(2 * time.Second)
	}
	dumpADLogs(t)
	t.Fatalf("KDC port %d not ready within %s", port, timeout)
}

// lookupGroupSID queries the running DC for a group's object SID via samba-tool.
func lookupGroupSID(t *testing.T, group string) string {
	t.Helper()
	out, err := exec.Command("docker", "exec", adContainerName,
		"samba-tool", "group", "show", group, "--attributes=objectSid").CombinedOutput()
	if err != nil {
		t.Fatalf("samba-tool group show %s: %v\n%s", group, err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(line), "objectsid:") {
			sid := strings.TrimSpace(line[len("objectsid:"):])
			if sid != "" {
				return sid
			}
		}
	}
	t.Fatalf("could not parse objectSid for group %s from:\n%s", group, out)
	return ""
}

func containsSID(sids []string, want string) bool {
	for _, s := range sids {
		if strings.EqualFold(s, want) {
			return true
		}
	}
	return false
}

func dumpADLogs(t *testing.T) {
	t.Helper()
	out, _ := exec.Command("docker", "logs", "--tail", "100", adContainerName).CombinedOutput()
	t.Logf("---- AD-DC container logs (tail) ----\n%s\n-------------------------------------", out)
}

// Real AD Ticket Generation

// getADAPREQ logs in as alice against the AD-DC and returns the raw AP-REQ
// bytes for the SMB SPN (NOT GSS-wrapped — KerberosService.Authenticate
// expects the raw AP-REQ).
func getADAPREQ(t *testing.T, krb5ConfPath string) []byte {
	t.Helper()

	cfg, err := krb5config.Load(krb5ConfPath)
	if err != nil {
		t.Fatalf("load krb5.conf: %v", err)
	}

	// kinit equivalent. DisablePAFXFAST avoids an unnecessary preauth round.
	cl := client.NewWithPassword(adUserAlice, adRealm, adUserPass, cfg, client.DisablePAFXFAST(true))
	if err := cl.Login(); err != nil {
		t.Fatalf("kinit as alice failed: %v", err)
	}
	defer cl.Destroy()
	t.Log("alice obtained a TGT from the AD-DC")

	// TGS-REQ for the SMB service principal.
	tkt, sessionKey, err := cl.GetServiceTicket(adSMBSPNShort)
	if err != nil {
		t.Fatalf("get service ticket for %s: %v", adSMBSPNShort, err)
	}
	t.Logf("alice obtained a service ticket for %s (enctype=%d)", adSMBSPNShort, sessionKey.KeyType)

	apReqBytes := buildAPREQ(t, cl, tkt, sessionKey)
	t.Logf("Built raw AP-REQ (%d bytes)", len(apReqBytes))
	return apReqBytes
}

// buildAPREQ marshals a raw AP-REQ for the given service ticket.
func buildAPREQ(t *testing.T, cl *client.Client, tkt messages.Ticket, sessionKey types.EncryptionKey) []byte {
	t.Helper()

	auth, err := types.NewAuthenticator(cl.Credentials.Domain(), cl.Credentials.CName())
	if err != nil {
		t.Fatalf("create authenticator: %v", err)
	}

	apReq, err := messages.NewAPReq(tkt, sessionKey, auth)
	if err != nil {
		t.Fatalf("build AP-REQ: %v", err)
	}

	apReqBytes, err := apReq.Marshal()
	if err != nil {
		t.Fatalf("marshal AP-REQ: %v", err)
	}
	// Sanity: a marshalled AP-REQ starts with APPLICATION 14 (0x6e).
	if len(apReqBytes) == 0 || apReqBytes[0] != 0x6e {
		t.Fatalf("unexpected AP-REQ prefix: %x", firstBytes(apReqBytes))
	}
	return apReqBytes
}

func firstBytes(b []byte) []byte {
	if len(b) > 8 {
		return b[:8]
	}
	return b
}
