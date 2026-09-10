package auth

import (
	"bytes"
	"crypto/hmac"
	"crypto/md5"
	"testing"
)

// buildSignedAuthenticate returns an AUTHENTICATE-shaped buffer with the MIC
// field set to the expected HMAC over the three handshake messages (the same
// computation VerifyAuthMessageMIC performs), so the test isolates the
// verify path, not the MIC derivation.
func buildSignedAuthenticate(t *testing.T, exportedSessionKey [16]byte, negotiate, challenge []byte) []byte {
	t.Helper()
	authMsg := validAuthenticateMessageForMIC()
	// Compute the MIC over the concatenation with the MIC zeroed.
	concat := bytes.Join([][]byte{negotiate, challenge, authMsg}, nil)
	clear(concat[len(concat)-len(authMsg)+authMicOffset:][:authMicSize])
	mac := hmac.New(md5.New, exportedSessionKey[:])
	mac.Write(concat)
	copy(authMsg[authMicOffset:authMicOffset+authMicSize], mac.Sum(nil))
	return authMsg
}

// validAuthenticateMessageForMIC returns a minimal AUTHENTICATE-message buffer
// long enough to carry the fixed MIC region (authMicOffset+authMicSize).
func validAuthenticateMessageForMIC() []byte {
	buf := make([]byte, authMicOffset+authMicSize)
	// MessageType 3 (AUTHENTICATE) at offset 8; IsValid only checks the
	// "NTLMSSP\0" prefix, so leave the signature zeroed unless needed.
	buf[8] = 3
	return buf
}

// TestVerifyAuthMessageMIC pins the AUTHENTICATE MIC check (MS-NLMP
// 3.2.5.2.1): a MIC computed over the three handshake messages with the MIC
// field zeroed verifies under the exported session key; a tampered MIC or a
// tampered message is rejected.
func TestVerifyAuthMessageMIC(t *testing.T) {
	var key [16]byte
	for i := range key {
		key[i] = byte(i + 3)
	}
	negotiate := []byte{0x01, 0x02, 0x03}
	challenge := []byte{0xAA, 0xBB, 0xCC, 0xDD}

	t.Run("AcceptsValidMIC", func(t *testing.T) {
		authMsg := buildSignedAuthenticate(t, key, negotiate, challenge)
		if err := VerifyAuthMessageMIC(key, negotiate, challenge, authMsg); err != nil {
			t.Errorf("VerifyAuthMessageMIC rejected a valid MIC: %v", err)
		}
	})

	t.Run("RejectsTamperedMIC", func(t *testing.T) {
		authMsg := buildSignedAuthenticate(t, key, negotiate, challenge)
		authMsg[authMicOffset+5] ^= 0xFF
		if err := VerifyAuthMessageMIC(key, negotiate, challenge, authMsg); err == nil {
			t.Error("VerifyAuthMessageMIC accepted a tampered MIC")
		}
	})

	t.Run("RejectsTamperedMessage", func(t *testing.T) {
		authMsg := buildSignedAuthenticate(t, key, negotiate, challenge)
		// Flip a byte outside the MIC region: the MIC no longer matches the
		// (tampered) message content.
		authMsg[12] ^= 0x01
		if err := VerifyAuthMessageMIC(key, negotiate, challenge, authMsg); err == nil {
			t.Error("VerifyAuthMessageMIC accepted a tampered message")
		}
	})
}

// TestVerifyNTLMSSPMechListMIC pins the NTLMSSP mechListMIC verify counterpart:
// a MIC computed by ComputeNTLMSSPMechListMIC verifies under the same inputs;
// a tampered MIC is rejected.
func TestVerifyNTLMSSPMechListMIC(t *testing.T) {
	var key [16]byte
	for i := range key {
		key[i] = byte(i + 7)
	}
	mechList := []byte{0x30, 0x0c, 0x06, 0x0a, 0x2b, 0x06, 0x01, 0x04, 0x01, 0x82, 0x37, 0x02, 0x02, 0x0a}
	flags := FlagExtendedSecurity | Flag128

	t.Run("AcceptsValidMIC", func(t *testing.T) {
		mic := ComputeNTLMSSPMechListMIC(key, mechList, flags, nil)
		if err := VerifyNTLMSSPMechListMIC(key, mechList, mic[:], flags); err != nil {
			t.Errorf("VerifyNTLMSSPMechListMIC rejected a valid MIC: %v", err)
		}
	})

	t.Run("RejectsTamperedMIC", func(t *testing.T) {
		mic := ComputeNTLMSSPMechListMIC(key, mechList, flags, nil)
		mic[3] ^= 0xFF
		if err := VerifyNTLMSSPMechListMIC(key, mechList, mic[:], flags); err == nil {
			t.Error("VerifyNTLMSSPMechListMIC accepted a tampered MIC")
		}
	})
}
