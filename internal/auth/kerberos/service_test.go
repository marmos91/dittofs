package kerberos

import (
	"testing"
	"time"

	"github.com/jcmturner/gokrb5/v8/crypto"
	"github.com/jcmturner/gokrb5/v8/iana/etypeID"
	"github.com/jcmturner/gokrb5/v8/messages"
	"github.com/jcmturner/gokrb5/v8/types"
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
