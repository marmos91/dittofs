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
	// The secure channel resolves the DC's SPN via the DC's own DNS (SRV locator),
	// so DNS (port 53) must be accepting queries before the first logon. Samba's
	// DNS can come up a beat after the RPC/SMB ports; without this wait the first
	// SRV lookup can hit "connection refused".
	waitForTCPAddr(t, dcIP+":53", 90*time.Second)

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

	// 1. Establish + first successful logon (builds the channel). Samba's SMB/RPC
	// stack can accept TCP on 135/445/53 a beat before it fully serves sessions
	// (transient "connection reset by peer" / handshake errors during DC
	// start-up), so retry the FIRST logon for a bounded window. This rides out DC
	// readiness only — it is not part of the hot-reload assertion.
	var res *netlogon.LogonResult
	var err error
	deadline := time.Now().Add(60 * time.Second)
	for {
		res, err = logonAlice()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pre-reload NETLOGON logon never succeeded within startup window: %v", err)
		}
		t.Logf("pre-reload logon not ready yet, retrying: %v", err)
		// Drop any half-open channel so the retry rebuilds cleanly.
		a.Reload(ctx)
		time.Sleep(3 * time.Second)
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

	// 3b. Concurrent logons WHILE a reload fires partway through: none may error,
	// proving the atomic teardown never races an in-flight SamLogon nor corrupts
	// the chained NETLOGON sequence number. A reload rebuilds the whole sealed
	// secure channel (a fresh ReqChallenge/Authenticate handshake), so this models
	// the realistic case — an admin changes the machine credential once — rather
	// than a pathological reload storm that would continuously reset the protocol
	// credential chain out from under every logon.
	const logonGoroutines = 6
	const logonsEach = 6
	var wg sync.WaitGroup
	errCh := make(chan error, logonGoroutines*logonsEach)

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

	// Fire a single reload in the middle of the concurrent logon burst.
	time.Sleep(30 * time.Millisecond)
	a.ReloadCredential(ctx, freshCred())
	t.Log("mid-burst ReloadCredential fired while concurrent logons are in flight")

	wg.Wait()
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
		t.Fatalf("%d concurrent logons errored across a mid-burst hot-reload (first: %v)", count, firstErr)
	}
	t.Logf("all %d concurrent logons succeeded across a mid-burst hot-reload", logonGoroutines*logonsEach)
}

// rotateMachinePassword changes the DITTOFS$ machine account password on the DC
// via samba-tool, so the test can prove a hot-reload makes a rebuilt secure
// channel authenticate with the NEW secret (and that the OLD secret stops
// working). MS-NRPC machine-account passwords have no history/min-age policy
// constraint here, so a direct setpassword is sufficient.
func rotateMachinePassword(t *testing.T, newPassword string) {
	t.Helper()
	out, err := exec.Command("docker", "exec", adContainerName,
		"samba-tool", "user", "setpassword", machineAccountName,
		"--newpassword="+newPassword,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("samba-tool setpassword %s failed: %v\n%s", machineAccountName, err, out)
	}
	t.Logf("rotated %s machine password on the DC", machineAccountName)
}

// TestNetlogonHotReload_PasswordRotation is the regression test for #1369
// Copilot finding #2: it proves a machine-password ROTATION reload makes the
// rebuilt secure channel authenticate with the NEW password — not a stale one
// frozen at process start.
//
// Earlier the machine credential was registered in go-msrpc under a process-wide
// sync.Once, so a rebuilt channel kept using the FIRST password even after a
// reload installed a new one. Supplying the credential per-connection via
// gssapi.WithCredential fixes that; this test would fail against the old code
// because the rebuilt channel would still present the original (now-wrong)
// secret and the DC would reject the SamLogon.
//
//  1. Logon alice successfully with the original machine password.
//  2. Rotate the DITTOFS$ password on the DC AND prove the OLD secret no longer
//     works: a ReloadCredential with the original (now-stale) password must make
//     the next logon FAIL (the rebuilt channel presents the wrong secret).
//  3. ReloadCredential with the NEW password — the next logon must SUCCEED on the
//     rebuilt channel, proving the new credential actually reached go-msrpc.
//
// Requires the AD-DC fixture (ad_dc build tag) and Docker.
func TestNetlogonHotReload_PasswordRotation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping NETLOGON password-rotation integration test in short mode")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found, skipping NETLOGON password-rotation test")
	}

	_, _, _, cleanup := setupADDC(t)
	defer cleanup()

	dcIP := getContainerIP(t)
	t.Logf("AD-DC container IP: %s", dcIP)
	waitForTCPAddr(t, dcIP+":135", 90*time.Second)
	waitForTCPAddr(t, dcIP+":445", 90*time.Second)
	waitForTCPAddr(t, dcIP+":53", 90*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	const newMachinePass = "RotatedMachinePass02!"

	credWith := func(password string) netlogon.MachineCredential {
		return netlogon.MachineCredential{
			AccountName: machineAccountName,
			Password:    password,
			Workstation: adDomain,
			DomainName:  adDomain,
			Realm:       adRealm,
			DCAddresses: []string{dcIP},
		}
	}

	prov := netlogon.NewMutableProvider(credWith(machineAccountPass))
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

	// 1. Establish + first successful logon with the ORIGINAL machine password,
	// retried through DC start-up readiness (not part of the rotation assertion).
	var res *netlogon.LogonResult
	var err error
	deadline := time.Now().Add(60 * time.Second)
	for {
		res, err = logonAlice()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pre-rotation NETLOGON logon never succeeded within startup window: %v", err)
		}
		t.Logf("pre-rotation logon not ready yet, retrying: %v", err)
		a.Reload(ctx)
		time.Sleep(3 * time.Second)
	}
	if res.UserSID == "" || len(res.GroupSIDs) == 0 {
		t.Fatalf("pre-rotation logon returned incomplete identity: %+v", res)
	}
	t.Logf("pre-rotation logon OK with original password: user_sid=%q", res.UserSID)

	// 2. Rotate the machine password on the DC. The ORIGINAL password is now stale.
	rotateMachinePassword(t, newMachinePass)

	// Prove the OLD secret no longer authenticates: reload with the now-stale
	// original password and confirm the next logon FAILS. This proves the rebuilt
	// channel presents whatever credential the reload installed (if it were frozen
	// at the first credential, both old and new would behave identically and this
	// signal would be meaningless).
	a.ReloadCredential(ctx, credWith(machineAccountPass))
	if _, errStale := logonAlice(); errStale == nil {
		t.Fatal("logon SUCCEEDED with the stale machine password after rotation — " +
			"the secure channel is not presenting the reloaded credential to the DC")
	} else {
		t.Logf("stale-password logon correctly rejected after rotation: %v", errStale)
	}

	// 3. Reload with the NEW password — the rebuilt channel must authenticate with
	// it and the logon must succeed. This is the core #1369 finding-#2 assertion:
	// the new credential reached go-msrpc (it was NOT frozen at the first one).
	a.ReloadCredential(ctx, credWith(newMachinePass))
	res2, err := logonAlice()
	if err != nil {
		t.Fatalf("post-rotation logon with the NEW machine password failed "+
			"(stale credential still in use?): %v", err)
	}
	if res2.UserSID != res.UserSID {
		t.Fatalf("post-rotation UserSID changed: pre=%q post=%q", res.UserSID, res2.UserSID)
	}
	t.Logf("post-rotation logon OK with NEW password on rebuilt channel: user_sid=%q", res2.UserSID)
}
