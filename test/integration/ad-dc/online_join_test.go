//go:build ad_dc

// Online-join integration test (#1323): the online MachineCredentialProvider
// creates a FRESH computer account in the Samba AD-DC over LDAP (no admin
// pre-provisioning), owns its machine password, and the NETLOGON secure channel
// authenticated with that just-created account validates a real domain user's
// NTLMv2 logon (member logon through the online-joined account).
//
// This is the online counterpart to TestNetlogonPassthroughAlice, which uses the
// admin-pre-provisioned DITTOFS$ account (offline provider). Here nothing is
// pre-provisioned: the provider does the `net ads join` equivalent itself.
//
// Run with: go test -tags=ad_dc -v -timeout 20m -run TestOnlineJoin ./test/integration/ad-dc/
// Requires Docker + host-routable container IP (CI Linux), like the other
// NETLOGON tests (EPM port 135 + dynamic RPC + 445 must be reachable without NAT).
package ad_dc_test

import (
	"context"
	"fmt"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/auth/netlogon"
	"github.com/oiweiwei/go-msrpc/ssp/credential"
	"github.com/oiweiwei/go-msrpc/ssp/ntlm"
)

// onlineJoinMachineName is a DISTINCT computer name from the fixture's
// pre-provisioned DITTOFS$, so the online join creates a genuinely new account.
const onlineJoinMachineName = "DITTOJOIN"

// memSecret is an in-memory netlogon.SecretStore for the integration test.
type memSecret struct {
	mu  sync.Mutex
	val string
}

func (m *memSecret) GetMachineSecret(context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.val, nil
}
func (m *memSecret) SetMachineSecret(_ context.Context, s string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.val = s
	return nil
}

// waitForDCStable blocks until the DC's NETLOGON/LDAP/KDC stack has been
// continuously serving for a short streak, riding out the entrypoint's
// provisioning-samba -> foreground-samba re-exec. It requires several
// consecutive successful `samba-tool processes` calls AND a reachable DNS (53)
// + SMB (445), since the secure channel and DC discovery use both.
func waitForDCStable(t *testing.T, timeout time.Duration) {
	t.Helper()
	const needStreak = 4
	deadline := time.Now().Add(timeout)
	streak := 0
	for time.Now().Before(deadline) {
		ok := exec.Command("docker", "exec", adContainerName, "samba-tool", "processes").Run() == nil
		if ok {
			streak++
			if streak >= needStreak {
				t.Logf("AD-DC stable (%d consecutive readiness checks)", streak)
				time.Sleep(2 * time.Second) // small extra settle
				return
			}
		} else {
			streak = 0
		}
		time.Sleep(2 * time.Second)
	}
	dumpADLogs(t)
	t.Fatalf("AD-DC did not stabilize within %s", timeout)
}

// TestOnlineJoinAndMemberLogon proves the full online-join lifecycle live:
//  1. The online provider creates the DITTOJOIN$ computer object over LDAP
//     (StartTLS) using the Administrator join credentials and sets its password.
//  2. The credential is persisted in the in-memory secret store.
//  3. A NETLOGON secure channel authenticated with the freshly-joined account
//     validates alice's NTLMv2 network logon and returns her SID + groups.
func TestOnlineJoinAndMemberLogon(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping online-join integration test in short mode")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found, skipping online-join test")
	}

	// Start the AD-DC fixture (reuses setupADDC from ad_dc_integration_test.go).
	_, _, _, cleanup := setupADDC(t)
	defer cleanup()

	dcIP := getContainerIP(t)
	t.Logf("AD-DC container IP: %s", dcIP)

	// NETLOGON needs EPM (135) + SMB (445); LDAP join needs 389 (StartTLS).
	waitForTCPAddr(t, dcIP+":135", 90*time.Second)
	waitForTCPAddr(t, dcIP+":445", 90*time.Second)
	waitForTCPAddr(t, dcIP+":389", 90*time.Second)

	// The fixture entrypoint provisions the domain, exports the keytab, then
	// STOPS the provisioning samba and re-execs it in the foreground. setupADDC
	// returns as soon as the keytab exists — i.e. right at that restart. Driving a
	// NETLOGON SamLogon through the just-joined account during that window yields
	// transient DNS/SMB failures (connection refused on :53, "logon is invalid"
	// on the SMB bind). Gate on the DC being CONTINUOUSLY ready (samba-tool
	// processes succeeds several times in a row) so join + logon run against a
	// stable DC, not the restart window.
	waitForDCStable(t, 4*time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	secret := &memSecret{}

	// Online-join config: create DITTOJOIN$ over StartTLS against the DC's LDAP,
	// binding as the domain Administrator (which can create computer objects).
	// The DC's self-signed cert is accepted via InsecureSkipVerify (lab fixture).
	cfg := netlogon.OnlineConfig{
		AccountName: onlineJoinMachineName + "$",
		Workstation: onlineJoinMachineName,
		DomainName:  adDomain,
		Realm:       adRealm,
		DCAddresses: []string{dcIP},
		Join: netlogon.JoinConfig{
			LDAPURL:      fmt.Sprintf("ldap://%s:389", dcIP),
			StartTLS:     true,
			BindDN:       adBindDN,
			BindPassword: adAdminPassword,
			BaseDN:       adBaseDN,
			MachineName:  onlineJoinMachineName,
			DNSHostName:  fmt.Sprintf("%s.%s", onlineJoinMachineName, "dittofs.ad"),
			SPNs:         []string{"HOST/" + onlineJoinMachineName + ".dittofs.ad"},
			TLS:          netlogon.JoinTLSConfig{InsecureSkipVerify: true},
		},
	}

	provider := netlogon.NewOnlineProvider(cfg, secret)
	auth := netlogon.NewAuthenticator(provider)
	defer auth.Close(ctx)

	// First Credential() triggers the online join (create computer + set pwd +
	// persist). Retry: the DC re-execs samba into the foreground shortly after
	// provisioning, so LDAP/SMB have a brief down window.
	var cred *netlogon.MachineCredential
	var err error
	for attempt := 1; attempt <= 15; attempt++ {
		cred, err = provider.Credential(ctx)
		if err == nil {
			break
		}
		t.Logf("online join attempt %d/15 not ready: %v", attempt, err)
		time.Sleep(3 * time.Second)
	}
	if err != nil {
		dumpADLogs(t)
		t.Fatalf("online join failed after retries: %v", err)
	}
	t.Logf("online join complete: account=%q password_len=%d", cred.AccountName, len(cred.Password))

	if secret.val == "" {
		t.Fatal("expected machine password to be persisted after join")
	}
	if secret.val != cred.Password {
		t.Fatal("persisted secret does not match the credential password")
	}

	// Confirm the computer object now exists in AD (best-effort: the entrypoint
	// re-execs samba into the foreground shortly after provisioning, so a
	// concurrent `docker exec` can be SIGTERM'd — that is informational only and
	// must not fail the test, which proves the join via the live logon below).
	if out, lerr := exec.Command("docker", "exec", adContainerName,
		"samba-tool", "computer", "list").CombinedOutput(); lerr != nil {
		t.Logf("samba-tool computer list (non-fatal): %v\n%s", lerr, out)
	} else {
		t.Logf("AD computer list after join:\n%s", out)
	}

	// Give the DC a moment to settle the freshly-set machine credential before
	// driving a NETLOGON SamLogon through it (Samba caches credentials briefly
	// after a password change).
	time.Sleep(5 * time.Second)

	// Now prove a member logon through the freshly-joined account: validate
	// alice's NTLMv2 response over the NETLOGON secure channel authenticated with
	// DITTOJOIN$.
	serverChallenge := [8]byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88}
	clientNonce := []byte{0xBB, 0xBB, 0xBB, 0xBB, 0xBB, 0xBB, 0xBB, 0xBB}

	// MsvAvNbComputerName MUST equal the NETLOGON secure-channel's NetBIOS
	// computer name (= the joined workstation, "DITTOJOIN"), NOT the domain name.
	// Samba's CVE-2022-38023 mitigation (NTLMv2_RESPONSE_verify_netlogon_creds,
	// MS-NRPC §3.5.4.5.1) parses this AV-pair out of the NTLMv2 NT-response blob
	// and rejects the SamLogon with STATUS_LOGON_FAILURE *before* authenticating
	// the user when it does not match the channel computer. In production DittoFS
	// advertises exactly this name in its NTLM Type-2 (SetADDomain → the
	// netbiosWorkstation name, the #1357 invariant), so a real domain client's
	// response always carries the matching value; the test must mirror that.
	v2 := &ntlm.V2{Config: &ntlm.Config{}}
	cm := &ntlm.ChallengeMessage{
		ServerChallenge: serverChallenge[:],
		TargetInfo: ntlm.AttrValues{
			ntlm.AttrNetBIOSDomainName:   &ntlm.Value{NetBIOSDomainName: adDomain},
			ntlm.AttrNetBIOSComputerName: &ntlm.Value{NetBIOSComputerName: onlineJoinMachineName},
		},
	}
	aliceCred := credential.NewFromPassword(adDomain+"\\"+adUserAlice, adUserPass)
	resp, cerr := v2.ChallengeResponse(ctx, aliceCred, cm, clientNonce)
	if cerr != nil {
		t.Fatalf("compute alice NTLMv2 response: %v", cerr)
	}

	var res *netlogon.LogonResult
	for attempt := 1; attempt <= 10; attempt++ {
		res, err = auth.NetworkLogon(ctx, netlogon.NetworkLogonRequest{
			Username:        adUserAlice,
			Domain:          adDomain,
			ServerChallenge: serverChallenge,
			NTResponse:      resp.NT,
			LMResponse:      resp.LM,
		})
		if err == nil {
			break
		}
		t.Logf("NetworkLogon attempt %d/10 via online-joined account: %v", attempt, err)
		time.Sleep(3 * time.Second)
	}
	if err != nil {
		dumpADLogs(t)
		t.Fatalf("member logon via online-joined account failed: %v", err)
	}

	t.Logf("member logon via online-joined account succeeded: user=%q user_sid=%q groups=%v",
		res.Username, res.UserSID, res.GroupSIDs)
	if res.UserSID == "" {
		t.Error("expected non-empty UserSID from DC")
	}
	if len(res.GroupSIDs) == 0 {
		t.Error("expected at least one GroupSID from DC")
	}
}
