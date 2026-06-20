//go:build ad_dc

// AD-4 integration test: ONE combined keytab serves both the SMB (cifs/) and
// NFS (nfs/) service principals, and the SMB layer is domain-aware.
//
// This builds on the same Samba AD-DC fixture as AD-1 (TestADGroupSIDsFromPAC):
// the fixture now registers BOTH cifs/ and nfs/ SPNs on the service account and
// exports their keys into a single dittofs.keytab. This test asserts, against
// that real fixture:
//
//  1. The combined keytab file actually contains BOTH a cifs/ and an nfs/
//     principal entry (so one keytab serves both protocols).
//  2. A Kerberos Provider built over that keytab and configured with the AD
//     domain identity (Realm/NetBIOSDomain) surfaces Realm()/NetBIOSDomain()/
//     DNSDomain() as configured/derived.
//  3. An SMB Handler with NetBIOSDomain set produces an NTLM Type-2 challenge
//     whose TargetInfo advertises the AD NetBIOS domain (MsvAvNbDomainName ==
//     "DITTOFS"), not the standalone "WORKGROUP" — exercising the exact
//     BuildChallenge call the handler's negotiate path makes.
//  4. As a real end-to-end SMB SPNEGO-Kerberos accept, alice's cifs/ service
//     ticket (obtained from the fixture KDC) is decrypted and validated by the
//     KerberosService built over the COMBINED keytab — proving the cifs/ key in
//     the shared keytab works for an SMB accept.
//
// The full live SMB+NFS krb5 session over the wire (real mount / smbclient with
// the same combined keytab serving both protocols simultaneously) is covered by
// AD-4b (scw / Windows acceptance); here we assert at the
// keytab + provider + challenge + krb5-accept level, which is deterministic in CI.
//
// Run with: go test -tags=ad_dc -v -timeout 20m ./test/integration/ad-dc/
package ad_dc_test

import (
	"encoding/binary"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	smbauth "github.com/marmos91/dittofs/internal/adapter/smb/auth"
	"github.com/marmos91/dittofs/internal/adapter/smb/handlers"
	kerbauth "github.com/marmos91/dittofs/internal/auth/kerberos"
	"github.com/marmos91/dittofs/pkg/auth/kerberos"
	dconfig "github.com/marmos91/dittofs/pkg/config"
)

// The combined keytab also carries nfs/dittofs.dittofs.ad@DITTOFS.AD (the NFS
// SPN) alongside adSMBSPNFull (cifs/...), defined in ad_dc_integration_test.go.
// Its presence is asserted at the keytab-byte level in (1); its live use is the
// NFS krb5 path / AD-4b.

// TestADCombinedKeytabAndDomainAwareSMB exercises the AD-4 wiring end to end
// against the Samba AD-DC fixture.
func TestADCombinedKeytabAndDomainAwareSMB(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping AD-DC integration test in short mode")
	}
	if !dockerAvailable() {
		t.Skip("docker not found, skipping AD-DC integration test")
	}

	// Bring up the fixture (shared helper from ad_dc_integration_test.go). It
	// builds the image, provisions the domain, and copies out the COMBINED
	// keytab + a host-side krb5.conf pointing at the mapped KDC port.
	_, keytabPath, krb5ConfPath, cleanup := setupADDC(t)
	defer cleanup()

	// --- (1) Combined keytab carries BOTH cifs/ and nfs/ principals ---------
	// The keytab v2 on-disk format stores each principal's name components as
	// length-prefixed ASCII, so the component label ("cifs" / "nfs") appears
	// verbatim in the file bytes. Asserting on the raw file (rather than poking
	// the keytab with a specific enctype) is robust to whatever enctypes the DC
	// happens to hold for the SPNs.
	ktBytes, err := os.ReadFile(keytabPath)
	if err != nil {
		t.Fatalf("read combined keytab %s: %v", keytabPath, err)
	}
	if !keytabHasComponent(ktBytes, "cifs") {
		t.Errorf("combined keytab missing cifs/ principal component (file %d bytes)", len(ktBytes))
	}
	if !keytabHasComponent(ktBytes, "nfs") {
		t.Errorf("combined keytab missing nfs/ principal component (file %d bytes)", len(ktBytes))
	}
	t.Logf("combined keytab (%d bytes) carries both cifs/ and nfs/ principals ✓", len(ktBytes))

	// --- (2) Provider surfaces the configured/derived AD domain identity ----
	// Configure Realm + NetBIOSDomain explicitly (the short name is never
	// derived). DNSDomain is left unset so the provider derives it from the
	// lowercased realm.
	cfg := &dconfig.KerberosConfig{
		Enabled:          true,
		KeytabPath:       keytabPath,
		ServicePrincipal: adSMBSPNFull, // cifs/...@DITTOFS.AD
		Krb5Conf:         krb5ConfPath,
		Realm:            adRealm,  // DITTOFS.AD
		NetBIOSDomain:    adDomain, // DITTOFS (short)
		MaxClockSkew:     5 * time.Minute,
		ContextTTL:       10 * time.Minute,
		MaxContexts:      100,
	}
	provider, err := kerberos.NewProvider(cfg)
	if err != nil {
		t.Fatalf("create kerberos provider over combined keytab: %v", err)
	}
	defer provider.Close()

	if got := provider.Realm(); got != adRealm {
		t.Errorf("Provider.Realm() = %q, want %q", got, adRealm)
	}
	if got := provider.NetBIOSDomain(); got != adDomain {
		t.Errorf("Provider.NetBIOSDomain() = %q, want %q", got, adDomain)
	}
	// DNSDomain was unset -> derived from the lowercased realm.
	wantDNS := strings.ToLower(adRealm) // dittofs.ad
	if got := provider.DNSDomain(); got != wantDNS {
		t.Errorf("Provider.DNSDomain() = %q, want derived %q", got, wantDNS)
	}
	t.Logf("Provider domain identity: realm=%q netbios=%q dns=%q ✓",
		provider.Realm(), provider.NetBIOSDomain(), provider.DNSDomain())

	// --- (3) Domain-aware SMB challenge advertises the AD NetBIOS domain ----
	// Build a Handler wired with the domain from the provider (the same wiring
	// the adapter performs), then make the exact BuildChallenge call its
	// negotiate path makes. The Type-2 TargetInfo must advertise the AD NetBIOS
	// domain, not the standalone "WORKGROUP".
	h := &handlers.Handler{
		NetBIOSDomain: provider.NetBIOSDomain(),
		DNSDomain:     provider.DNSDomain(),
	}
	challenge, _ := smbauth.BuildChallenge(h.NetBIOSDomain, h.DNSDomain)
	pairs := parseChallengeTargetInfo(t, challenge)

	if got := pairs[smbauth.AvNbDomainName]; got != adDomain {
		t.Errorf("Type-2 MsvAvNbDomainName = %q, want %q (domain-aware, not WORKGROUP)", got, adDomain)
	}
	if got := pairs[smbauth.AvDnsDomainName]; got != wantDNS {
		t.Errorf("Type-2 MsvAvDnsDomainName = %q, want %q", got, wantDNS)
	}
	t.Logf("SMB Type-2 challenge advertises NbDomain=%q DnsDomain=%q ✓",
		pairs[smbauth.AvNbDomainName], pairs[smbauth.AvDnsDomainName])

	// --- (4) Real SMB SPNEGO-Kerberos accept with the combined keytab -------
	// alice obtains a cifs/ service ticket from the fixture KDC; the
	// KerberosService built over the COMBINED keytab must decrypt + validate it.
	// This proves the cifs/ key inside the shared keytab actually serves SMB
	// accepts (the nfs/ key is exercised by the NFS krb5 path / AD-4b).
	service := kerbauth.NewKerberosService(provider)
	apReqBytes := getADAPREQ(t, krb5ConfPath) // alice -> cifs/ AP-REQ

	result, err := service.Authenticate(apReqBytes, adSMBSPNFull)
	if err != nil {
		t.Fatalf("SMB krb5 accept with combined keytab failed: %v", err)
	}
	if result.Principal == "" {
		t.Error("expected non-empty principal from SMB krb5 accept")
	}
	if !strings.EqualFold(result.Realm, adRealm) {
		t.Errorf("accepted realm = %q, want %q", result.Realm, adRealm)
	}
	t.Logf("SMB krb5 accept via combined keytab: principal=%q realm=%q ✓",
		result.Principal, result.Realm)
}

// dockerAvailable reports whether a docker binary is on PATH (mirrors the
// skip-if-docker-absent guard in ad_dc_integration_test.go).
func dockerAvailable() bool {
	_, err := exec.LookPath("docker")
	return err == nil
}

// keytabHasComponent reports whether the raw keytab file contains a principal
// name component equal to want. In the MIT keytab v2 format a name component is
// serialized as a uint16 length (big-endian) followed by that many ASCII bytes;
// scanning for the length-prefixed token avoids false positives from substrings
// embedded in other fields (e.g. the realm or a longer hostname).
func keytabHasComponent(buf []byte, want string) bool {
	w := []byte(want)
	n := len(w)
	if n == 0 || n > 0xFFFF {
		return false
	}
	for i := 0; i+2+n <= len(buf); i++ {
		if int(binary.BigEndian.Uint16(buf[i:i+2])) != n {
			continue
		}
		if string(buf[i+2:i+2+n]) == want {
			return true
		}
	}
	return false
}

// parseChallengeTargetInfo extracts the TargetInfo AV_PAIR blob from a Type-2
// CHALLENGE message and decodes it into AvID -> UTF-16LE string. The TargetInfo
// fields live at offset 40 of the Type-2 message: len(uint16)@40, off(uint32)@44.
func parseChallengeTargetInfo(t *testing.T, msg []byte) map[smbauth.AvID]string {
	t.Helper()
	const tiLenOff = 40
	const tiOffOff = 44
	if len(msg) < tiOffOff+4 {
		t.Fatalf("challenge too short (%d bytes) to hold TargetInfo fields", len(msg))
	}
	tiLen := int(binary.LittleEndian.Uint16(msg[tiLenOff : tiLenOff+2]))
	tiOff := int(binary.LittleEndian.Uint32(msg[tiOffOff : tiOffOff+4]))
	if tiOff+tiLen > len(msg) {
		t.Fatalf("TargetInfo off=%d len=%d overruns message (%d bytes)", tiOff, tiLen, len(msg))
	}
	return decodeAvPairs(t, msg[tiOff:tiOff+tiLen])
}

// decodeAvPairs walks an AV_PAIR list returning AvID -> decoded UTF-16LE string
// (string-valued pairs only; it stops at AvEOL).
func decodeAvPairs(t *testing.T, buf []byte) map[smbauth.AvID]string {
	t.Helper()
	out := make(map[smbauth.AvID]string)
	for len(buf) >= 4 {
		id := smbauth.AvID(binary.LittleEndian.Uint16(buf[0:2]))
		ln := int(binary.LittleEndian.Uint16(buf[2:4]))
		if id == smbauth.AvEOL {
			break
		}
		if 4+ln > len(buf) {
			t.Fatalf("AV_PAIR id=0x%x len %d overruns (%d remaining)", id, ln, len(buf)-4)
		}
		val := buf[4 : 4+ln]
		runes := make([]uint16, ln/2)
		for i := 0; i < ln/2; i++ {
			runes[i] = binary.LittleEndian.Uint16(val[i*2:])
		}
		out[id] = string(utf16Runes(runes))
		buf = buf[4+ln:]
	}
	return out
}

func utf16Runes(u []uint16) []rune {
	r := make([]rune, len(u))
	for i, c := range u {
		r[i] = rune(c)
	}
	return r
}
