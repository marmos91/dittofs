//go:build ad_dc

package ad_dc_test

import (
	"context"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/auth/netlogon"
	"github.com/oiweiwei/go-msrpc/ssp/credential"
	"github.com/oiweiwei/go-msrpc/ssp/ntlm"
)

// machineAccountName is the sAMAccountName created by entrypoint.sh via
// "samba-tool computer create dittofs --computerpassword=MachinePass01!".
// AD renders the computer-create name as "<name>$" in sAMAccountName.
const (
	machineAccountName = "DITTOFS$"
	machineAccountPass = "MachinePass01!"
)

// getContainerIP returns the internal Docker bridge IP of adContainerName.
// In CI (Linux) this IP is directly reachable from the host, which is required
// for NETLOGON: the endpoint mapper (port 135) and dynamic RPC ports must all
// be reachable without NAT. On macOS/Docker Desktop container IPs are not
// host-reachable, so the NetworkLogon call times out and the test FAILS there
// (it does not skip) — run this only in CI (Linux) or inside a Linux VM.
func getContainerIP(t *testing.T) string {
	t.Helper()
	// Newer Docker engines drop the legacy top-level .NetworkSettings.IPAddress
	// field, so a {{json .NetworkSettings.IPAddress}} template errors out; use the
	// per-network Networks map, which is populated on the default bridge on both
	// old and new engines. In CI (Linux) this bridge IP is host-reachable, which
	// NETLOGON requires (EPM port 135 + the dynamic RPC port, no NAT).
	// Emit one IP per line so multiple attached networks don't concatenate into
	// a single invalid address; the AD-DC fixture uses only the default bridge,
	// so the first non-empty line is the bridge IP we want.
	out, err := exec.Command("docker", "inspect",
		"--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}\n{{end}}",
		adContainerName,
	).Output()
	if err != nil {
		t.Fatalf("docker inspect network IP: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if ip := strings.TrimSpace(line); ip != "" {
			return ip
		}
	}
	t.Fatal("could not determine container IP address from docker inspect")
	return ""
}

// TestNetlogonPassthroughAlice validates the full NETLOGON passthrough path:
// the Authenticator opens a sealed NETLOGON secure channel to the AD-DC using
// the machine account DITTOFS$ and calls NetrLogonSamLogon with alice's NTLMv2
// NT response. The DC must return alice's UserSID and at least one GroupSID.
//
// This test requires the AD-DC fixture (ad_dc build tag) and Docker.
func TestNetlogonPassthroughAlice(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping NETLOGON passthrough integration test in short mode")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found, skipping NETLOGON passthrough test")
	}

	// setupADDC starts the Samba AD-DC container (or reuses it if already running).
	// We only need the keytab/krb5conf paths here; the DC address comes from docker inspect.
	_, _, _, cleanup := setupADDC(t)
	defer cleanup()

	dcIP := getContainerIP(t)
	t.Logf("AD-DC container IP: %s", dcIP)

	// Wait for the DC's DCE-RPC endpoint mapper (port 135) AND the SMB server
	// (port 445) to accept connections. Samba's RPC stack comes up shortly after
	// the KDC; dialing too early yields "connection refused" on the NETLOGON bind.
	// The schannel rides a Kerberos SMB session over ncacn_np (\PIPE\netlogon), so
	// 445 must be listening too — it can bind a beat after 135 (#1345).
	waitForTCPAddr(t, dcIP+":135", 90*time.Second)
	waitForTCPAddr(t, dcIP+":445", 90*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Build the Authenticator with the machine account created in entrypoint.sh.
	// DCAddresses uses the container's internal IP so that EPM (port 135) and the
	// dynamically assigned NETLOGON RPC port are reachable without host-side NAT.
	a := netlogon.NewAuthenticator(netlogon.NewOfflineProvider(netlogon.MachineCredential{
		AccountName: machineAccountName,
		Password:    machineAccountPass,
		Workstation: adDomain,
		DomainName:  adDomain,
		Realm:       adRealm,
		DCAddresses: []string{dcIP},
	}))
	defer a.Close(ctx)

	// Compute alice's NTLMv2 NT and LM responses for a fixed 8-byte server
	// challenge using go-msrpc's ntlm.V2.ChallengeResponse.
	//
	// ChallengeMessage is built with the minimum fields required for NTLMv2:
	//   - ServerChallenge: the 8-byte challenge DittoFS would have sent on the wire.
	//   - TargetInfo: contains the domain and computer NetBIOS names, which are
	//     used to build the NTLMv2ClientChallenge blob in the NT response.
	//
	// The nonce (client challenge) is also fixed so the test is deterministic.
	serverChallenge := [8]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	clientNonce := []byte{0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA}

	v2 := &ntlm.V2{Config: &ntlm.Config{}}
	cm := &ntlm.ChallengeMessage{
		ServerChallenge: serverChallenge[:],
		TargetInfo: ntlm.AttrValues{
			ntlm.AttrNetBIOSDomainName:   &ntlm.Value{NetBIOSDomainName: adDomain},
			ntlm.AttrNetBIOSComputerName: &ntlm.Value{NetBIOSComputerName: adDomain},
		},
	}

	// credential.NewFromPassword("Domain\\User", password) is the go-msrpc form
	// for a domain user credential (same pattern as the ntlm v2_test.go example).
	cred := credential.NewFromPassword(adDomain+"\\"+adUserAlice, adUserPass)

	resp, err := v2.ChallengeResponse(ctx, cred, cm, clientNonce)
	if err != nil {
		t.Fatalf("compute NTLMv2 ChallengeResponse: %v", err)
	}

	// NetworkLogon sends the NTLMv2 responses to the DC for validation.
	res, err := a.NetworkLogon(ctx, netlogon.NetworkLogonRequest{
		Username:        adUserAlice,
		Domain:          adDomain,
		ServerChallenge: serverChallenge,
		NTResponse:      resp.NT,
		LMResponse:      resp.LM,
	})
	if err != nil {
		t.Fatalf("NETLOGON passthrough failed: %v", err)
	}

	t.Logf("NETLOGON passthrough succeeded: user=%q domain=%q user_sid=%q group_sids=%v",
		res.Username, res.DomainName, res.UserSID, res.GroupSIDs)

	if res.UserSID == "" {
		t.Error("expected non-empty UserSID from DC, got empty")
	}
	if len(res.GroupSIDs) == 0 {
		t.Error("expected at least one GroupSID from DC, got none")
	}
}

// waitForTCPAddr blocks until addr accepts a TCP connection or the timeout
// elapses (fatal on timeout). Used to wait for the DC's DCE-RPC endpoint
// mapper (port 135), which comes up shortly after the KDC.
func waitForTCPAddr(t *testing.T, addr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("timed out waiting for %s to accept connections (DC RPC endpoint-mapper not reachable)", addr)
}
