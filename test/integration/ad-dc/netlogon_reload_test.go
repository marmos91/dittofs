//go:build ad_dc

package ad_dc_test

import (
	"context"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/auth/netlogon"
	"github.com/oiweiwei/go-msrpc/ssp/credential"
	"github.com/oiweiwei/go-msrpc/ssp/ntlm"
)

// ntlmResponseFor computes a deterministic NTLMv2 NT/LM response for the given
// AD user against a fixed 8-byte server challenge, mirroring the helper inlined
// in TestNetlogonPassthroughAlice.
func ntlmResponseFor(ctx context.Context, t *testing.T, user, pass string, serverChallenge [8]byte) (nt, lm []byte) {
	t.Helper()
	clientNonce := []byte{0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA}
	v2 := &ntlm.V2{Config: &ntlm.Config{}}
	cm := &ntlm.ChallengeMessage{
		ServerChallenge: serverChallenge[:],
		TargetInfo: ntlm.AttrValues{
			ntlm.AttrNetBIOSDomainName:   &ntlm.Value{NetBIOSDomainName: adDomain},
			ntlm.AttrNetBIOSComputerName: &ntlm.Value{NetBIOSComputerName: adDomain},
		},
	}
	cred := credential.NewFromPassword(adDomain+"\\"+user, pass)
	resp, err := v2.ChallengeResponse(ctx, cred, cm, clientNonce)
	if err != nil {
		t.Fatalf("compute NTLMv2 ChallengeResponse for %q: %v", user, err)
	}
	return resp.NT, resp.LM
}

// TestNetlogonHotReload proves the NETLOGON machine credential / DC binding can
// be hot-reloaded without a restart (#1325):
//
//  1. Build an Authenticator over a MutableProvider, open the secure channel and
//     do a successful SamLogon for alice.
//  2. Fire a ReloadCredential with a fresh (still-valid) machine credential — the
//     same path the SMB adapter takes on an identity-provider config change. This
//     tears down the cached channel atomically.
//  3. Prove the NEXT SamLogon rebuilds the channel and still succeeds (alice),
//     and that a burst of concurrent logons issued WHILE reloads fire never error
//     or corrupt the chained sequence number.
//
// Requires the AD-DC fixture (ad_dc build tag) and Docker.
func TestNetlogonHotReload(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping NETLOGON hot-reload integration test in short mode")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found, skipping NETLOGON hot-reload test")
	}

	_, _, _, cleanup := setupADDC(t)
	defer cleanup()

	dcIP := getContainerIP(t)
	t.Logf("AD-DC container IP: %s", dcIP)
	waitForTCPAddr(t, dcIP+":135", 90*time.Second)
	waitForTCPAddr(t, dcIP+":445", 90*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	freshCred := func() netlogon.MachineCredential {
		return netlogon.MachineCredential{
			AccountName: machineAccountName,
			Password:    machineAccountPass,
			Workstation: adDomain,
			DomainName:  adDomain,
			Realm:       adRealm,
			DCAddresses: []string{dcIP},
		}
	}

	prov := netlogon.NewMutableProvider(freshCred())
	a := netlogon.NewAuthenticator(prov)
	defer a.Close(ctx)

	serverChallenge := [8]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	aliceNT, aliceLM := ntlmResponseFor(ctx, t, adUserAlice, adUserPass, serverChallenge)

	logonAlice := func() (*netlogon.LogonResult, error) {
		return a.NetworkLogon(ctx, netlogon.NetworkLogonRequest{
			Username:        adUserAlice,
			Domain:          adDomain,
			ServerChallenge: serverChallenge,
			NTResponse:      aliceNT,
			LMResponse:      aliceLM,
		})
	}

	// 1. Establish + first successful logon (builds the channel).
	res, err := logonAlice()
	if err != nil {
		t.Fatalf("pre-reload NETLOGON logon failed: %v", err)
	}
	if res.UserSID == "" || len(res.GroupSIDs) == 0 {
		t.Fatalf("pre-reload logon returned incomplete identity: %+v", res)
	}
	t.Logf("pre-reload logon OK: user_sid=%q groups=%d", res.UserSID, len(res.GroupSIDs))

	// 2. Hot-reload the credential (tears down the cached channel atomically).
	a.ReloadCredential(ctx, freshCred())
	t.Log("ReloadCredential fired: cached secure channel torn down")

	// 3a. Next logon must rebuild the channel and still succeed.
	res2, err := logonAlice()
	if err != nil {
		t.Fatalf("post-reload NETLOGON logon failed (channel did not rebuild): %v", err)
	}
	if res2.UserSID != res.UserSID {
		t.Fatalf("post-reload UserSID changed: pre=%q post=%q", res.UserSID, res2.UserSID)
	}
	t.Logf("post-reload logon OK on rebuilt channel: user_sid=%q", res2.UserSID)

	// 3b. Concurrent logons WHILE reloads fire: none may error, proving the
	// rebuild never races an in-flight SamLogon or corrupts the sequence number.
	const logonGoroutines = 6
	const logonsEach = 5
	var wg sync.WaitGroup
	errCh := make(chan error, logonGoroutines*logonsEach)

	stop := make(chan struct{})
	var reloadWG sync.WaitGroup
	reloadWG.Add(1)
	go func() {
		defer reloadWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
				a.ReloadCredential(ctx, freshCred())
				time.Sleep(20 * time.Millisecond)
			}
		}
	}()

	for g := 0; g < logonGoroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < logonsEach; i++ {
				if _, lerr := logonAlice(); lerr != nil {
					errCh <- lerr
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	reloadWG.Wait()
	close(errCh)

	var firstErr error
	count := 0
	for e := range errCh {
		count++
		if firstErr == nil {
			firstErr = e
		}
	}
	if count != 0 {
		t.Fatalf("%d concurrent logons errored during hot-reload (first: %v)", count, firstErr)
	}
	t.Logf("all %d concurrent logons succeeded during continuous hot-reload", logonGoroutines*logonsEach)
}
