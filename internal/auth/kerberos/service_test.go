package kerberos

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jcmturner/gofork/encoding/asn1"
	"github.com/jcmturner/gokrb5/v8/asn1tools"
	"github.com/jcmturner/gokrb5/v8/crypto"
	"github.com/jcmturner/gokrb5/v8/iana"
	"github.com/jcmturner/gokrb5/v8/iana/addrtype"
	"github.com/jcmturner/gokrb5/v8/iana/asnAppTag"
	"github.com/jcmturner/gokrb5/v8/iana/errorcode"
	"github.com/jcmturner/gokrb5/v8/iana/etypeID"
	"github.com/jcmturner/gokrb5/v8/iana/keyusage"
	"github.com/jcmturner/gokrb5/v8/iana/nametype"
	"github.com/jcmturner/gokrb5/v8/keytab"
	"github.com/jcmturner/gokrb5/v8/messages"
	"github.com/jcmturner/gokrb5/v8/types"

	pkgkerberos "github.com/marmos91/dittofs/pkg/auth/kerberos"
	"github.com/marmos91/dittofs/pkg/config"
)

func TestAuthResult_SessionKeyPreference(t *testing.T) {
	// When authenticator has a subkey, AuthResult.SessionKey should be the subkey
	subkey := types.EncryptionKey{
		KeyType:  etypeID.AES256_CTS_HMAC_SHA1_96,
		KeyValue: []byte("subkey-value-32-bytes-long------"),
	}

	result := &AuthResult{
		Principal:  "alice",
		Realm:      "EXAMPLE.COM",
		SessionKey: subkey, // subkey preferred over ticket session key
	}

	if result.SessionKey.KeyType != etypeID.AES256_CTS_HMAC_SHA1_96 {
		t.Fatalf("expected AES256 key type, got %d", result.SessionKey.KeyType)
	}
}

func TestBuildMutualAuth(t *testing.T) {
	svc := &KerberosService{
		replayCache: NewReplayCache(5 * time.Minute),
	}

	sessionKey := types.EncryptionKey{
		KeyType:  etypeID.AES128_CTS_HMAC_SHA1_96,
		KeyValue: make([]byte, 16),
	}

	tests := []struct {
		name   string
		subKey types.EncryptionKey
		cusec  int
	}{
		{
			name:  "WithoutSubkey",
			cusec: 42,
		},
		{
			name: "WithSubkey",
			subKey: types.EncryptionKey{
				KeyType:  etypeID.AES256_CTS_HMAC_SHA1_96,
				KeyValue: make([]byte, 32),
			},
			cusec: 99,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			apReq := &messages.APReq{
				Authenticator: types.Authenticator{
					CTime:  time.Now().UTC(),
					Cusec:  tt.cusec,
					SubKey: tt.subKey,
				},
			}

			apRepBytes, err := svc.BuildMutualAuth(apReq, sessionKey)
			if err != nil {
				t.Fatalf("BuildMutualAuth failed: %v", err)
			}

			if len(apRepBytes) == 0 {
				t.Fatal("expected non-empty AP-REP bytes")
			}

			// Raw AP-REP starts with APPLICATION 15 tag (0x6F), not GSS wrapper (0x60)
			if apRepBytes[0] != 0x6F {
				t.Fatalf("expected APPLICATION 15 tag (0x6F), got 0x%02X", apRepBytes[0])
			}
		})
	}
}

// TestBuildMutualAuth_OmitsAcceptorSubkey is the #1250 regression guard: even
// when the client authenticator carries a subkey, the AP-REP must NOT echo it as
// an acceptor subkey. An asserted AP-REP subkey puts the GSS context into the
// acceptor-subkey regime (RFC 4121 Section 4.2.6.1), obliging every per-message
// token to set the AcceptorSubkey flag (0x04) — which our SentByAcceptor-only
// MIC/Wrap tokens do not — so a real Windows/Samba client rejects the server
// mechListMIC with NT_STATUS_ACCESS_DENIED.
func TestBuildMutualAuth_OmitsAcceptorSubkey(t *testing.T) {
	svc := &KerberosService{replayCache: NewReplayCache(5 * time.Minute)}
	sessionKey := types.EncryptionKey{
		KeyType:  etypeID.AES128_CTS_HMAC_SHA1_96,
		KeyValue: make([]byte, 16),
	}
	apReq := &messages.APReq{
		Authenticator: types.Authenticator{
			CTime: time.Now().UTC(),
			Cusec: 7,
			SubKey: types.EncryptionKey{
				KeyType:  etypeID.AES256_CTS_HMAC_SHA1_96,
				KeyValue: make([]byte, 32),
			},
		},
	}

	apRepBytes, err := svc.BuildMutualAuth(apReq, sessionKey)
	if err != nil {
		t.Fatalf("BuildMutualAuth failed: %v", err)
	}

	var apRep messages.APRep
	if err := apRep.Unmarshal(apRepBytes); err != nil {
		t.Fatalf("unmarshal AP-REP: %v", err)
	}

	decrypted, err := crypto.DecryptEncPart(apRep.EncPart, sessionKey, keyUsageAPRepEncPart)
	if err != nil {
		t.Fatalf("decrypt EncAPRepPart: %v", err)
	}

	var encPart messages.EncAPRepPart
	if err := encPart.Unmarshal(decrypted); err != nil {
		t.Fatalf("unmarshal EncAPRepPart: %v", err)
	}

	if encPart.Subkey.KeyType != 0 || len(encPart.Subkey.KeyValue) != 0 {
		t.Errorf("AP-REP must not echo an acceptor subkey, got KeyType=%d KeyLen=%d",
			encPart.Subkey.KeyType, len(encPart.Subkey.KeyValue))
	}
}

func TestNewKerberosService_NilProvider(t *testing.T) {
	// Should handle nil provider gracefully (replay cache still works)
	svc := NewKerberosService(nil)
	if svc == nil {
		t.Fatal("expected non-nil KerberosService even with nil provider")
	}
	if svc.replayCache == nil {
		t.Fatal("expected non-nil replay cache")
	}
}

func TestKerberosService_Provider(t *testing.T) {
	svc := NewKerberosService(nil)
	if svc.Provider() != nil {
		t.Fatal("expected nil provider")
	}
}

func TestKerberosHostAddress(t *testing.T) {
	tests := []struct {
		name       string
		clientAddr string
		want       string
	}{
		{name: "IPv4 remote address", clientAddr: "192.168.1.10:445", want: "192.168.1.10"},
		{name: "bare IPv4", clientAddr: "10.0.0.5", want: "10.0.0.5"},
		{name: "IPv6 remote address", clientAddr: "[2001:db8::1]:445", want: "2001:db8::1"},
		{name: "scoped IPv6 remote address", clientAddr: "[fe80::1%lo0]:445", want: "fe80::1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := kerberosHostAddress(tt.clientAddr)
			if err != nil {
				t.Fatalf("kerberosHostAddress: %v", err)
			}
			if net.IP(got.Address).String() != tt.want {
				t.Fatalf("address = %v, want %s", net.IP(got.Address), tt.want)
			}
		})
	}
	if _, err := kerberosHostAddress("not-an-ip"); err == nil {
		t.Fatal("expected invalid client address to be rejected")
	}
}

func TestTicketAllowsClientIP(t *testing.T) {
	netBIOS := types.HostAddress{AddrType: addrtype.NetBios, Address: []byte("WINCLIENT       ")}
	peer := types.HostAddressFromNetIP(net.ParseIP("192.168.1.10"))
	other := types.HostAddressFromNetIP(net.ParseIP("10.0.0.5"))
	tests := []struct {
		name   string
		caddr  []types.HostAddress
		wantOK bool
	}{
		{name: "NetBIOS only", caddr: []types.HostAddress{netBIOS}, wantOK: true},
		{name: "empty", caddr: nil, wantOK: true},
		{name: "matching IPv4", caddr: []types.HostAddress{peer}, wantOK: true},
		{name: "mismatched IPv4", caddr: []types.HostAddress{other}, wantOK: false},
		{name: "NetBIOS and matching IPv4", caddr: []types.HostAddress{netBIOS, peer}, wantOK: true},
		{name: "NetBIOS and mismatched IPv4", caddr: []types.HostAddress{netBIOS, other}, wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ticketAllowsClientIP(peer, tt.caddr)
			if got != tt.wantOK {
				t.Fatalf("ticketAllowsClientIP = %v, want %v", got, tt.wantOK)
			}
		})
	}
}

func TestFormatKerberosHostAddresses(t *testing.T) {
	addresses := []types.HostAddress{
		{AddrType: addrtype.NetBios, Address: []byte("WINCLIENT       ")},
		types.HostAddressFromNetIP(net.ParseIP("192.168.1.10")),
	}
	got := formatKerberosHostAddresses(addresses)
	if len(got) != 2 || got[0] != "NETBIOS:WINCLIENT" || got[1] != "192.168.1.10" {
		t.Fatalf("formatted addresses = %v", got)
	}
}

func TestHasSubkey(t *testing.T) {
	tests := []struct {
		name   string
		subKey types.EncryptionKey
		want   bool
	}{
		{"ValidSubkey", types.EncryptionKey{KeyType: etypeID.AES128_CTS_HMAC_SHA1_96, KeyValue: []byte("some-key-value")}, true},
		{"EmptySubkey", types.EncryptionKey{}, false},
		{"ZeroKeyType", types.EncryptionKey{KeyType: 0, KeyValue: []byte("some-value")}, false},
		{"ZeroLengthValue", types.EncryptionKey{KeyType: etypeID.AES128_CTS_HMAC_SHA1_96, KeyValue: nil}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			apReq := &messages.APReq{
				Authenticator: types.Authenticator{SubKey: tt.subKey},
			}
			if got := HasSubkey(apReq); got != tt.want {
				t.Errorf("HasSubkey() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestAuthenticateFromClient_AddressBind runs a real AP-REQ through
// VerifyAPREQ. A NetBIOS-only caddr makes the first check return
// KRB_AP_ERR_BADADDR; authentication still succeeds only if that error is
// recognized and the second verify runs. A ticket that lists a different IP
// must still be rejected.
func TestAuthenticateFromClient_AddressBind(t *testing.T) {
	const (
		realm    = "EXAMPLE.COM"
		svcName  = "cifs/fileserver.example.com"
		spn      = svcName + "@" + realm
		password = "test-password"
		kvno     = 1
		client   = "192.168.1.10:445"
	)
	kt := keytab.New()
	if err := kt.AddEntry(svcName, realm, password, time.Now(), kvno, etypeID.AES128_CTS_HMAC_SHA1_96); err != nil {
		t.Fatal(err)
	}
	svc := serviceFromKeytab(t, kt, spn)

	t.Run("NetBIOS only", func(t *testing.T) {
		raw := marshalAPReq(t, kt, []types.HostAddress{{
			AddrType: addrtype.NetBios,
			Address:  []byte("WINCLIENT       "),
		}})
		res, err := svc.AuthenticateFromClient(raw, spn, client)
		if err != nil {
			t.Fatalf("AuthenticateFromClient: %v", err)
		}
		if res.Principal != "alice" {
			t.Fatalf("principal = %q, want alice", res.Principal)
		}
	})

	t.Run("mismatched IP", func(t *testing.T) {
		raw := marshalAPReq(t, kt, []types.HostAddress{
			types.HostAddressFromNetIP(net.ParseIP("10.0.0.5")),
		})
		_, err := svc.AuthenticateFromClient(raw, spn, client)
		var krbErr messages.KRBError
		if !errors.As(err, &krbErr) || krbErr.ErrorCode != errorcode.KRB_AP_ERR_BADADDR {
			t.Fatalf("error = %v, want KRB_AP_ERR_BADADDR", err)
		}
	})
}

func serviceFromKeytab(t *testing.T, kt *keytab.Keytab, spn string) *KerberosService {
	t.Helper()
	dir := t.TempDir()
	data, err := kt.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	keytabPath := filepath.Join(dir, "svc.keytab")
	if err := os.WriteFile(keytabPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	confPath := filepath.Join(dir, "krb5.conf")
	conf := "[libdefaults]\n default_realm = EXAMPLE.COM\n dns_lookup_kdc = false\n"
	if err := os.WriteFile(confPath, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	provider, err := pkgkerberos.NewProvider(&config.KerberosConfig{
		Enabled:          true,
		KeytabPath:       keytabPath,
		ServicePrincipal: spn,
		Krb5Conf:         confPath,
		MaxClockSkew:     5 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	return NewKerberosService(provider)
}

func marshalAPReq(t *testing.T, kt *keytab.Keytab, caddr []types.HostAddress) []byte {
	t.Helper()
	const (
		realm   = "EXAMPLE.COM"
		svcName = "cifs/fileserver.example.com"
		kvno    = 1
	)
	etype := etypeID.AES128_CTS_HMAC_SHA1_96
	sname := types.NewPrincipalName(nametype.KRB_NT_PRINCIPAL, svcName)
	cname := types.NewPrincipalName(nametype.KRB_NT_PRINCIPAL, "alice")
	now := time.Now().UTC()

	impl, err := crypto.GetEtype(etype)
	if err != nil {
		t.Fatal(err)
	}
	sessionKey, err := types.GenerateEncryptionKey(impl)
	if err != nil {
		t.Fatal(err)
	}
	part := messages.EncTicketPart{
		Flags:     types.NewKrbFlags(),
		Key:       sessionKey,
		CRealm:    realm,
		CName:     cname,
		AuthTime:  now,
		StartTime: now,
		EndTime:   now.Add(time.Hour),
		CAddr:     caddr,
	}
	rawPart, err := asn1.Marshal(part)
	if err != nil {
		t.Fatal(err)
	}
	rawPart = asn1tools.AddASNAppTag(rawPart, asnAppTag.EncTicketPart)
	skey, _, err := kt.GetEncryptionKey(sname, realm, kvno, etype)
	if err != nil {
		t.Fatal(err)
	}
	encPart, err := crypto.GetEncryptedData(rawPart, skey, keyusage.KDC_REP_TICKET, kvno)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := types.NewAuthenticator(realm, cname)
	if err != nil {
		t.Fatal(err)
	}
	apReq, err := messages.NewAPReq(messages.Ticket{
		TktVNO:  iana.PVNO,
		Realm:   realm,
		SName:   sname,
		EncPart: encPart,
	}, sessionKey, auth)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := apReq.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
