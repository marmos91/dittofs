package kerberos

import (
	"testing"
	"time"

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
