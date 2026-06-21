//go:build ad_dc

// AD-2 (#1234) integration test: the LDAP/AD identity provider resolves a real
// Active Directory user against the Samba AD-DC fixture (the same fixture AD-1
// uses for PAC testing). It asserts:
//
//   - RFC2307 idmap: alice's uidNumber/gidNumber (10001/10000) are read from the
//     directory.
//   - Nested groups: alice ∈ devs, and devs ⊂ engineering, so the resolved GIDs
//     include BOTH groups' gidNumbers — resolved server-side via the AD
//     LDAP_MATCHING_RULE_IN_CHAIN matching rule.
//   - The connection is encrypted (StartTLS); a plaintext bind is refused.
//
// The provider binds as the domain Administrator service account and searches
// the directory; no Kerberos ticket is involved (that is AD-1's path).
//
// Run with: go test -tags=ad_dc -v -timeout 20m -run TestLDAP ./test/integration/ad-dc/
// Requires: Docker (the samba-ad-dc image builds + provisions under linux/amd64
// emulation on Apple Silicon, which is slow — hence the generous timeout).
package ad_dc_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/identity"
	"github.com/marmos91/dittofs/pkg/identity/ldap"
)

const (
	adAdminPassword = "Passw0rd!2024"
	adBindDN        = "CN=Administrator,CN=Users,DC=dittofs,DC=ad"
	adBaseDN        = "DC=dittofs,DC=ad"

	// Expected RFC2307 attrs stamped on alice by the fixture entrypoint.
	aliceUIDNumber = 10001
	aliceGIDNumber = 10000
)

// TestLDAPResolveDomainUser exercises the full AD-2 path against the AD-DC
// fixture: bind, find alice, read RFC2307 UID/GID, resolve nested groups.
func TestLDAPResolveDomainUser(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping AD-DC LDAP integration test in short mode")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found, skipping AD-DC LDAP integration test")
	}

	ldapPort, cleanup := setupADDCForLDAP(t)
	defer cleanup()

	// StartTLS over 389 with the fixture's self-signed cert. Encryption is
	// REQUIRED by the provider (plaintext is refused); insecure_skip_verify
	// accepts the lab DC's self-signed certificate.
	cfg := &ldap.Config{
		Enabled:      true,
		URL:          fmt.Sprintf("ldap://127.0.0.1:%d", ldapPort),
		StartTLS:     true,
		BaseDN:       adBaseDN,
		BindDN:       adBindDN,
		BindPassword: adAdminPassword,
		UserAttr:     "sAMAccountName",
		Realm:        adRealm,
		Idmap:        ldap.IdmapRFC2307,
		NestedGroups: true,
		Timeout:      15 * time.Second,
		TLS:          ldap.TLSConfig{InsecureSkipVerify: true},
	}

	provider, err := ldap.New(cfg, nil, nil)
	if err != nil {
		t.Fatalf("ldap.New: %v", err)
	}

	// The deadline must exceed the whole retry window below (up to 15 attempts ×
	// (15s per-attempt LDAP timeout + 2s backoff)), otherwise the context would
	// expire mid-loop and the remaining attempts would fail with a misleading
	// "context deadline exceeded" instead of the real connectivity error.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Retry on transient connect errors: the entrypoint re-execs samba into the
	// foreground shortly after provisioning, so LDAP (389) has a brief down
	// window right when the test first connects ("connection reset by peer").
	var res *identity.ResolvedIdentity
	for attempt := 1; attempt <= 15; attempt++ {
		if ctx.Err() != nil {
			break
		}
		res, err = provider.Resolve(ctx, &identity.Credential{ExternalID: "alice@" + adRealm})
		if err == nil {
			break
		}
		t.Logf("Resolve(alice) attempt %d/15 not ready yet: %v", attempt, err)
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		dumpADLogs(t)
		t.Fatalf("Resolve(alice) failed after retries: %v", err)
	}
	t.Logf("Resolved alice: %+v", res)

	if !res.Found {
		t.Fatal("expected alice to resolve (Found=true)")
	}
	if res.UID != aliceUIDNumber {
		t.Errorf("UID = %d, want %d (RFC2307 uidNumber)", res.UID, aliceUIDNumber)
	}
	if res.GID != aliceGIDNumber {
		t.Errorf("GID = %d, want %d (RFC2307 gidNumber)", res.GID, aliceGIDNumber)
	}
	if !strings.EqualFold(res.Domain, adRealm) {
		t.Errorf("Domain = %q, want %q", res.Domain, adRealm)
	}

	// Nested groups: alice ∈ devs, devs ⊂ engineering. Both groups' GIDs must be
	// present. The fixture stamps a gidNumber on neither group by default, so the
	// provider falls back to RID-derived GIDs for groups without gidNumber — what
	// matters here is that BOTH devs and engineering surface as distinct GIDs,
	// proving the LDAP_MATCHING_RULE_IN_CHAIN nested walk resolved the transitive
	// set (a non-nested walk would surface devs only).
	devsGID := lookupGroupRIDAsGID(t, "devs")
	engGID := lookupGroupRIDAsGID(t, "engineering")
	if !containsGID(res.GIDs, devsGID) {
		t.Errorf("GIDs %v missing devs GID %d", res.GIDs, devsGID)
	}
	if !containsGID(res.GIDs, engGID) {
		t.Errorf("GIDs %v missing nested engineering GID %d", res.GIDs, engGID)
	}
	t.Logf("Nested groups resolved: devs=%d engineering=%d (both present) ✓", devsGID, engGID)
}

// TestLDAPPlaintextRefused asserts the provider refuses a plaintext bind when
// neither StartTLS nor allow_plaintext is configured.
func TestLDAPPlaintextRefused(t *testing.T) {
	cfg := &ldap.Config{
		Enabled:      true,
		URL:          "ldap://127.0.0.1:389",
		BaseDN:       adBaseDN,
		BindDN:       adBindDN,
		BindPassword: adAdminPassword,
	}
	if _, err := ldap.New(cfg, nil, nil); err == nil {
		t.Fatal("expected ldap.New to refuse plaintext config without start_tls/allow_plaintext")
	}
}

// setupADDCForLDAP builds + runs the AD-DC fixture mapping the LDAP port (389)
// and waits for it to accept connections. It reuses the fixture's entrypoint
// (which provisions the domain + alice/bob + devs⊂engineering).
func setupADDCForLDAP(t *testing.T) (ldapPort int, cleanup func()) {
	t.Helper()

	_ = exec.Command("docker", "rm", "-f", adContainerName).Run()

	dockerfileDir := findADDockerfileDir(t)

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

	t.Log("Starting AD-DC container (LDAP)...")
	// --privileged: domain provision sets the sysvol NT ACL (smbd.set_nt_acl),
	// which is denied on an overlayfs-backed container FS (e.g. Docker Desktop on
	// macOS). Keeps the fixture portable across developer machines (issue #1252).
	runOut, err := exec.Command("docker", "run", "-d",
		"--platform", "linux/amd64",
		"--privileged",
		"--name", adContainerName,
		"-p", "0:389/tcp",
		adImageName,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run failed: %v\n%s", err, runOut)
	}

	portOut, err := exec.Command("docker", "port", adContainerName, "389/tcp").Output()
	if err != nil {
		dumpADLogs(t)
		t.Fatalf("docker port: %v", err)
	}
	parts := strings.Split(strings.TrimSpace(string(portOut)), ":")
	ldapPort, err = strconv.Atoi(parts[len(parts)-1])
	if err != nil {
		t.Fatalf("parse port %q: %v", portOut, err)
	}
	t.Logf("AD-DC LDAP mapped to host port %d", ldapPort)

	// Provisioning a fresh AD domain under emulation is slow.
	t.Log("Waiting for LDAP TCP port...")
	waitForADPort(t, ldapPort, 6*time.Minute)

	// alice's RFC2307 attrs + group nesting are stamped after the LDAP stack is
	// up; wait until alice is queryable so the test sees a fully provisioned dir.
	t.Log("Waiting for alice provisioning...")
	waitForADUser(t, "alice", 4*time.Minute)

	cleanup = func() {
		t.Log("Cleaning up AD-DC container...")
		_ = exec.Command("docker", "rm", "-f", adContainerName).Run()
	}
	return ldapPort, cleanup
}

// waitForADUser polls until samba-tool reports the given user exists.
func waitForADUser(t *testing.T, user string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := exec.Command("docker", "exec", adContainerName,
			"samba-tool", "user", "show", user).Run(); err == nil {
			return
		}
		time.Sleep(2 * time.Second)
	}
	dumpADLogs(t)
	t.Fatalf("user %s not provisioned within %s", user, timeout)
}

// lookupGroupRIDAsGID returns the RID (final SID sub-authority) of a group,
// which is what the provider's RID-fallback idmap derives as the group's GID
// when the group carries no gidNumber. Parsed from the dotted SID string.
func lookupGroupRIDAsGID(t *testing.T, group string) uint32 {
	t.Helper()
	sidStr := lookupGroupSID(t, group)
	idx := strings.LastIndex(sidStr, "-")
	if idx < 0 || idx == len(sidStr)-1 {
		t.Fatalf("cannot parse RID from SID %q", sidStr)
	}
	rid, err := strconv.ParseUint(sidStr[idx+1:], 10, 32)
	if err != nil {
		t.Fatalf("parse RID from %q: %v", sidStr, err)
	}
	return uint32(rid)
}

func containsGID(gids []uint32, want uint32) bool {
	for _, g := range gids {
		if g == want {
			return true
		}
	}
	return false
}
