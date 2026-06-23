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
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/auth/netlogon"
	"github.com/oiweiwei/go-msrpc/ssp/credential"
	"github.com/oiweiwei/go-msrpc/ssp/ntlm"

	"os/exec"
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

	// Confirm the computer object now exists in AD.
	out, lerr := exec.Command("docker", "exec", adContainerName,
		"samba-tool", "computer", "list").CombinedOutput()
	if lerr != nil {
		t.Fatalf("samba-tool computer list: %v\n%s", lerr, out)
	}
	t.Logf("AD computer list after join:\n%s", out)

	// Now prove a member logon through the freshly-joined account: validate
	// alice's NTLMv2 response over the NETLOGON secure channel authenticated with
	// DITTOJOIN$.
	serverChallenge := [8]byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88}
	clientNonce := []byte{0xBB, 0xBB, 0xBB, 0xBB, 0xBB, 0xBB, 0xBB, 0xBB}

	v2 := &ntlm.V2{Config: &ntlm.Config{}}
	cm := &ntlm.ChallengeMessage{
		ServerChallenge: serverChallenge[:],
		TargetInfo: ntlm.AttrValues{
			ntlm.AttrNetBIOSDomainName:   &ntlm.Value{NetBIOSDomainName: adDomain},
			ntlm.AttrNetBIOSComputerName: &ntlm.Value{NetBIOSComputerName: adDomain},
		},
	}
	aliceCred := credential.NewFromPassword(adDomain+"\\"+adUserAlice, adUserPass)
	resp, cerr := v2.ChallengeResponse(ctx, aliceCred, cm, clientNonce)
	if cerr != nil {
		t.Fatalf("compute alice NTLMv2 response: %v", cerr)
	}

	var res *netlogon.LogonResult
	for attempt := 1; attempt <= 5; attempt++ {
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
		t.Logf("NetworkLogon attempt %d/5 via online-joined account: %v", attempt, err)
		time.Sleep(2 * time.Second)
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
