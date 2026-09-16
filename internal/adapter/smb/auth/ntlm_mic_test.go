package auth

import (
	"bytes"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rc4"
	"encoding/binary"
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

// clientMechListMIC computes a mechListMIC the way a CLIENT does, written out
// from MS-NLMP 3.4.5.2/3.4.5.3 rather than by calling the code under test. That
// independence is the whole point: the verifier used to be checked only against
// the server's own emitter, and because the two agreed, nobody noticed they
// agreed on the wrong direction's keys. A client signs with the
// client-to-server signing and sealing keys, so nothing a real client sends
// could ever verify.
func clientMechListMIC(exportedSessionKey [16]byte, mechList []byte, flags NegotiateFlag) []byte {
	sh := md5.New()
	sh.Write(exportedSessionKey[:])
	sh.Write([]byte("session key to client-to-server signing key magic constant\x00"))
	signKey := sh.Sum(nil)

	mac := hmac.New(md5.New, signKey)
	mac.Write(make([]byte, 4)) // SeqNum = 0
	mac.Write(mechList)
	checksum := mac.Sum(nil)[:8]

	if flags&FlagKeyExch != 0 {
		var sealInput []byte
		switch {
		case flags&Flag128 != 0:
			sealInput = exportedSessionKey[:16]
		case flags&Flag56 != 0:
			sealInput = exportedSessionKey[:7]
		default:
			sealInput = exportedSessionKey[:5]
		}
		kh := md5.New()
		kh.Write(sealInput)
		kh.Write([]byte("session key to client-to-server sealing key magic constant\x00"))
		c, err := rc4.NewCipher(kh.Sum(nil))
		if err != nil {
			panic(err)
		}
		sealed := make([]byte, 8)
		c.XORKeyStream(sealed, checksum)
		checksum = sealed
	}

	mic := make([]byte, 16)
	binary.LittleEndian.PutUint32(mic[0:4], 0x00000001)
	copy(mic[4:12], checksum)
	return mic
}

// TestVerifyNTLMSSPMechListMIC pins the NTLMSSP mechListMIC verify path against
// a client-side computation written independently of the emitter, with and
// without KEY_EXCH.
func TestVerifyNTLMSSPMechListMIC(t *testing.T) {
	var key [16]byte
	for i := range key {
		key[i] = byte(i + 7)
	}
	mechList := []byte{0x30, 0x0c, 0x06, 0x0a, 0x2b, 0x06, 0x01, 0x04, 0x01, 0x82, 0x37, 0x02, 0x02, 0x0a}

	for _, tc := range []struct {
		name  string
		flags NegotiateFlag
	}{
		{"NoKeyExch", FlagExtendedSecurity | Flag128},
		{"KeyExch128", FlagExtendedSecurity | Flag128 | FlagKeyExch},
		{"KeyExch40", FlagExtendedSecurity | FlagKeyExch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mic := clientMechListMIC(key, mechList, tc.flags)
			if err := VerifyNTLMSSPMechListMIC(key, mechList, mic, tc.flags); err != nil {
				t.Errorf("rejected the MIC a client actually sends: %v", err)
			}

			mic[5] ^= 0xFF
			if err := VerifyNTLMSSPMechListMIC(key, mechList, mic, tc.flags); err == nil {
				t.Error("accepted a tampered MIC")
			}
		})
	}
}

// TestVerifyNTLMSSPMechListMIC_RejectsTheServersOwnDirection is the regression
// that the round-trip test could not express. The server emits its own
// mechListMIC with the server-to-client keys, and verifying a client's MIC with
// those same keys rejects every legitimate client — so the verifier must NOT
// accept what the emitter produces.
func TestVerifyNTLMSSPMechListMIC_RejectsTheServersOwnDirection(t *testing.T) {
	var key [16]byte
	for i := range key {
		key[i] = byte(i + 7)
	}
	mechList := []byte{0x30, 0x0c, 0x06, 0x0a, 0x2b, 0x06, 0x01, 0x04, 0x01, 0x82, 0x37, 0x02, 0x02, 0x0a}
	flags := FlagExtendedSecurity | Flag128 | FlagKeyExch

	serverMIC := ComputeNTLMSSPMechListMIC(key, mechList, flags, nil)
	if err := VerifyNTLMSSPMechListMIC(key, mechList, serverMIC, flags); err == nil {
		t.Fatal("the verifier accepted the server's own outbound MIC, so it is deriving " +
			"client-to-server keys from the server-to-client constants: every client that " +
			"sends a mechListMIC is rejected")
	}
}
