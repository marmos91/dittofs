package auth

import (
	"bytes"
	"crypto/hmac"
	"crypto/md5" //nolint:gosec // MD5 is required for NTLM protocol testing
	"crypto/rc4" //nolint:gosec // RC4 required to reproduce NTLMSSP seal in the oracle
	"encoding/binary"
	"testing"
)

// TestComputeNTLMSSPMechListMIC exercises ComputeNTLMSSPMechListMIC against
// an independent first-principles reconstruction of MS-NLMP 3.4.5.2 (SIGNKEY),
// 3.4.5.3 (SEALKEY), 3.4.4.2 (NTLM2 signature with/without KEY_EXCH), and
// 2.2.2.9.1 (NTLMSSP_MESSAGE_SIGNATURE layout).
//
// It covers all four variants of the sealing-key derivation (no KEY_EXCH,
// 40-bit truncation, 56-bit truncation, 128-bit full-key) — the first
// attempt at #371 failed because we always used the full 16 bytes; Samba's
// smbtorture client does not advertise NEGOTIATE_128 and falls into the
// 40-bit branch.
func TestComputeNTLMSSPMechListMIC(t *testing.T) {
	exportedSessionKey := [16]byte{
		0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77,
		0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff,
	}
	// DER SEQUENCE OF OID with just the NTLMSSP OID — matches what a minimal
	// Windows / Samba SPNEGO NegTokenInit carries.
	mechListBytes := []byte{
		0x30, 0x0c, 0x06, 0x0a,
		0x2b, 0x06, 0x01, 0x04, 0x01, 0x82, 0x37, 0x02, 0x02, 0x0a,
	}

	// Common HMAC input: SigningKey is always derived from the full
	// ExportedSessionKey regardless of the seal-key truncation mode.
	signMD5 := md5.New()
	signMD5.Write(exportedSessionKey[:])
	signMD5.Write([]byte(serverSignMagic))
	expectedSignKey := signMD5.Sum(nil)
	mac := hmac.New(md5.New, expectedSignKey)
	mac.Write([]byte{0, 0, 0, 0})
	mac.Write(mechListBytes)
	hmacChecksum := mac.Sum(nil)[:8]

	sealedWith := func(sealInput []byte) []byte {
		sh := md5.New()
		sh.Write(sealInput)
		sh.Write([]byte(serverSealMagic))
		sealKey := sh.Sum(nil)
		cipher, err := rc4.NewCipher(sealKey)
		if err != nil {
			t.Fatalf("rc4 init: %v", err)
		}
		sealed := make([]byte, 8)
		cipher.XORKeyStream(sealed, hmacChecksum)
		return sealed
	}

	pack := func(checksum []byte) []byte {
		out := make([]byte, 16)
		binary.LittleEndian.PutUint32(out[0:4], 1)
		copy(out[4:12], checksum)
		return out
	}

	cases := []struct {
		name         string
		flags        NegotiateFlag
		wantChecksum []byte
	}{
		{
			name:         "NoKeyExch_PlainHMAC",
			flags:        FlagNTLM,
			wantChecksum: hmacChecksum,
		},
		{
			name:         "KeyExch_40bit_FirstBranch",
			flags:        FlagNTLM | FlagKeyExch,
			wantChecksum: sealedWith(exportedSessionKey[:5]),
		},
		{
			name:         "KeyExch_56bit",
			flags:        FlagNTLM | FlagKeyExch | Flag56,
			wantChecksum: sealedWith(exportedSessionKey[:7]),
		},
		{
			name:         "KeyExch_128bit",
			flags:        FlagNTLM | FlagKeyExch | Flag128,
			wantChecksum: sealedWith(exportedSessionKey[:16]),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expected := pack(tc.wantChecksum)
			got := ComputeNTLMSSPMechListMIC(exportedSessionKey, mechListBytes, tc.flags, nil)
			if len(got) != 16 {
				t.Fatalf("expected 16-byte signature, got %d", len(got))
			}
			if !bytes.Equal(got, expected) {
				t.Errorf("mechListMIC mismatch\nexpected %x\n     got %x", expected, got)
			}
			if ver := binary.LittleEndian.Uint32(got[0:4]); ver != 1 {
				t.Errorf("Version = 0x%08x, want 0x00000001", ver)
			}
			if seq := binary.LittleEndian.Uint32(got[12:16]); seq != 0 {
				t.Errorf("SeqNum = 0x%08x, want 0x00000000", seq)
			}
		})
	}

	// Sealing must actually cover mechListBytes — guards against no-op bugs.
	t.Run("MICCoversInput", func(t *testing.T) {
		flags := FlagNTLM | FlagKeyExch
		a := ComputeNTLMSSPMechListMIC(exportedSessionKey, mechListBytes, flags, nil)
		b := ComputeNTLMSSPMechListMIC(exportedSessionKey, append([]byte{0xff}, mechListBytes...), flags, nil)
		if bytes.Equal(a[4:12], b[4:12]) {
			t.Error("checksum did not change when mechListBytes changed")
		}
	})

	// Debug struct gets populated with all intermediates when provided.
	t.Run("DebugStructPopulated", func(t *testing.T) {
		var dbg NTLMSSPMechListMICDebug
		mic := ComputeNTLMSSPMechListMIC(exportedSessionKey, mechListBytes, FlagNTLM|FlagKeyExch, &dbg)
		if !bytes.Equal(dbg.MIC[:], mic) {
			t.Error("dbg.MIC does not match returned mic")
		}
		if !bytes.Equal(dbg.SigningKey[:], expectedSignKey) {
			t.Errorf("dbg.SigningKey = %x, want %x", dbg.SigningKey, expectedSignKey)
		}
		if dbg.HMACChecksum == [8]byte{} {
			t.Error("dbg.HMACChecksum is zero")
		}
	})
}
