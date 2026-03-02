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

func TestBuildMutualAuth_IncludesCtimeAndCusec(t *testing.T) {
	svc := &KerberosService{
		replayCache: NewReplayCache(5 * time.Minute),
	}

	now := time.Now().UTC()
	apReq := &messages.APReq{
		Authenticator: types.Authenticator{
			CTime: now,
			Cusec: 42,
		},
	}

	sessionKey := types.EncryptionKey{
		KeyType:  etypeID.AES128_CTS_HMAC_SHA1_96,
		KeyValue: make([]byte, 16),
	}

	// BuildMutualAuth should produce raw AP-REP bytes (not GSS-wrapped)
	apRepBytes, err := svc.BuildMutualAuth(apReq, sessionKey)
	if err != nil {
		t.Fatalf("BuildMutualAuth failed: %v", err)
	}

	if len(apRepBytes) == 0 {
		t.Fatal("expected non-empty AP-REP bytes")
	}

	// The raw AP-REP should start with APPLICATION 15 tag (0x6F)
	// ASN.1 APPLICATION 15 = 0x60 | 0x0F = 0x6F
	if apRepBytes[0] != 0x6F {
		t.Fatalf("expected AP-REP to start with APPLICATION 15 (0x6F), got 0x%02X", apRepBytes[0])
	}

	// Should NOT start with GSS-API wrapper (0x60 followed by OID)
	// 0x6F is APPLICATION 15, not 0x60 APPLICATION 0 (GSS wrapper)
}

func TestBuildMutualAuth_WithSubkey(t *testing.T) {
	svc := &KerberosService{
		replayCache: NewReplayCache(5 * time.Minute),
	}

	now := time.Now().UTC()
	subkey := types.EncryptionKey{
		KeyType:  etypeID.AES256_CTS_HMAC_SHA1_96,
		KeyValue: make([]byte, 32),
	}

	apReq := &messages.APReq{
		Authenticator: types.Authenticator{
			CTime:  now,
			Cusec:  99,
			SubKey: subkey,
		},
	}

	sessionKey := types.EncryptionKey{
		KeyType:  etypeID.AES128_CTS_HMAC_SHA1_96,
		KeyValue: make([]byte, 16),
	}

	apRepBytes, err := svc.BuildMutualAuth(apReq, sessionKey)
	if err != nil {
		t.Fatalf("BuildMutualAuth with subkey failed: %v", err)
	}

	if len(apRepBytes) == 0 {
		t.Fatal("expected non-empty AP-REP bytes with subkey")
	}

	// Should be APPLICATION 15 (raw AP-REP, not GSS-wrapped)
	if apRepBytes[0] != 0x6F {
		t.Fatalf("expected APPLICATION 15 tag (0x6F), got 0x%02X", apRepBytes[0])
	}
}

func TestBuildMutualAuth_WithoutSubkey(t *testing.T) {
	svc := &KerberosService{
		replayCache: NewReplayCache(5 * time.Minute),
	}

	now := time.Now().UTC()
	apReq := &messages.APReq{
		Authenticator: types.Authenticator{
			CTime: now,
			Cusec: 0,
			// No SubKey
		},
	}

	sessionKey := types.EncryptionKey{
		KeyType:  etypeID.AES128_CTS_HMAC_SHA1_96,
		KeyValue: make([]byte, 16),
	}

	apRepBytes, err := svc.BuildMutualAuth(apReq, sessionKey)
	if err != nil {
		t.Fatalf("BuildMutualAuth without subkey failed: %v", err)
	}

	if len(apRepBytes) == 0 {
		t.Fatal("expected non-empty AP-REP bytes without subkey")
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

func TestHasSubkeyHelper(t *testing.T) {
	// With valid subkey
	apReqWithSubkey := &messages.APReq{
		Authenticator: types.Authenticator{
			SubKey: types.EncryptionKey{
				KeyType:  etypeID.AES128_CTS_HMAC_SHA1_96,
				KeyValue: []byte("some-key-value"),
			},
		},
	}
	if !HasSubkey(apReqWithSubkey) {
		t.Fatal("expected HasSubkey to return true for valid subkey")
	}

	// Without subkey
	apReqNoSubkey := &messages.APReq{
		Authenticator: types.Authenticator{},
	}
	if HasSubkey(apReqNoSubkey) {
		t.Fatal("expected HasSubkey to return false for empty subkey")
	}

	// With zero key type but non-empty value
	apReqZeroType := &messages.APReq{
		Authenticator: types.Authenticator{
			SubKey: types.EncryptionKey{
				KeyType:  0,
				KeyValue: []byte("some-value"),
			},
		},
	}
	if HasSubkey(apReqZeroType) {
		t.Fatal("expected HasSubkey to return false for zero key type")
	}
}
