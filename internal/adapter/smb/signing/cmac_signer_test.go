package signing

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// RFC 4493 Section 4 test vectors.
// Key: 2b7e151628aed2a6abf7158809cf4f3c
var cmacTestKey = mustDecodeHex("2b7e151628aed2a6abf7158809cf4f3c")

func mustDecodeHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// TestCMAC_RFC4493_Vectors tests AES-CMAC against all RFC 4493 Section 4 test vectors.
func TestCMAC_RFC4493_Vectors(t *testing.T) {
	signer := NewCMACSigner(cmacTestKey)
	if signer == nil {
		t.Fatal("NewCMACSigner returned nil")
	}

	tests := []struct {
		name        string
		messageHex  string
		expectedHex string
	}{
		{
			name:        "empty message",
			messageHex:  "",
			expectedHex: "bb1d6929e95937287fa37d129b756746",
		},
		{
			name:        "16-byte message",
			messageHex:  "6bc1bee22e409f96e93d7e117393172a",
			expectedHex: "070a16b46b4d4144f79bdd9dd04a287c",
		},
		{
			name:        "40-byte message",
			messageHex:  "6bc1bee22e409f96e93d7e117393172aae2d8a571e03ac9c9eb76fac45af8e5130c81c46a35ce411",
			expectedHex: "dfa66747de9ae63030ca32611497c827",
		},
		{
			name:        "64-byte message",
			messageHex:  "6bc1bee22e409f96e93d7e117393172aae2d8a571e03ac9c9eb76fac45af8e5130c81c46a35ce411e5fbc1191a0a52eff69f2445df4f9b17ad2b417be66c3710",
			expectedHex: "51f0bebf7e3b9d92fc49741779363cfe",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var msg []byte
			if tt.messageHex != "" {
				msg = mustDecodeHex(tt.messageHex)
			}
			expected := mustDecodeHex(tt.expectedHex)
			mac := signer.cmacMAC(msg)

			if !bytes.Equal(mac[:], expected) {
				t.Errorf("CMAC %s:\n  got:  %x\n  want: %x", tt.name, mac, expected)
			}
		})
	}
}

// TestCMAC_Subkeys verifies the subkey generation against RFC 4493 Section 4.
// K1 = fbeed618357133667c85e08f7236a8de
// K2 = f7ddac306ae266ccf90bc11ee46d513b
func TestCMAC_Subkeys(t *testing.T) {
	signer := NewCMACSigner(cmacTestKey)
	if signer == nil {
		t.Fatal("NewCMACSigner returned nil")
	}

	expectedK1 := mustDecodeHex("fbeed618357133667c85e08f7236a8de")
	expectedK2 := mustDecodeHex("f7ddac306ae266ccf90bc11ee46d513b")

	if !bytes.Equal(signer.k1[:], expectedK1) {
		t.Errorf("K1 mismatch:\n  got:  %x\n  want: %x", signer.k1, expectedK1)
	}
	if !bytes.Equal(signer.k2[:], expectedK2) {
		t.Errorf("K2 mismatch:\n  got:  %x\n  want: %x", signer.k2, expectedK2)
	}
}

// TestCMACSigner_SMBSign tests the Sign method which zeros the signature field
// before computing the CMAC.
func TestCMACSigner_SMBSign(t *testing.T) {
	signer := NewCMACSigner(cmacTestKey)

	// Create a minimal SMB2 message (header only)
	message := make([]byte, SMB2HeaderSize+20)
	message[0], message[1], message[2], message[3] = 0xFE, 'S', 'M', 'B'
	for i := SMB2HeaderSize; i < len(message); i++ {
		message[i] = byte(i)
	}

	sig := signer.Sign(message)

	// Signature should be non-zero
	var zero [SignatureSize]byte
	if bytes.Equal(sig[:], zero[:]) {
		t.Error("Sign() returned zero signature")
	}

	// Deterministic
	sig2 := signer.Sign(message)
	if !bytes.Equal(sig[:], sig2[:]) {
		t.Error("Sign() is not deterministic")
	}
}

// TestCMACSigner_Verify tests signature verification.
func TestCMACSigner_Verify(t *testing.T) {
	signer := NewCMACSigner(cmacTestKey)

	message := make([]byte, SMB2HeaderSize+20)
	message[0], message[1], message[2], message[3] = 0xFE, 'S', 'M', 'B'
	for i := SMB2HeaderSize; i < len(message); i++ {
		message[i] = byte(i)
	}

	// Sign in place using SignMessage helper
	SignMessage(signer, message)

	// Verify should pass
	if !signer.Verify(message) {
		t.Error("Verify() failed for correctly signed message")
	}

	// Tamper and verify should fail
	tampered := make([]byte, len(message))
	copy(tampered, message)
	tampered[SMB2HeaderSize] ^= 0xFF
	if signer.Verify(tampered) {
		t.Error("Verify() passed for tampered message")
	}
}
