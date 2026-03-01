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

// TestCMAC_RFC4493_EmptyMessage tests AES-CMAC with an empty message.
// Expected MAC: bb1d6929e95937287fa37d129b756746
func TestCMAC_RFC4493_EmptyMessage(t *testing.T) {
	signer := NewCMACSigner(cmacTestKey)
	if signer == nil {
		t.Fatal("NewCMACSigner returned nil")
	}

	// For CMAC raw computation, we need a message at least SMB2HeaderSize.
	// But RFC 4493 test vectors test raw AES-CMAC, not SMB signing.
	// We test the raw cmacMAC function directly.
	mac := signer.cmacMAC([]byte{})
	expected := mustDecodeHex("bb1d6929e95937287fa37d129b756746")

	if !bytes.Equal(mac[:], expected) {
		t.Errorf("CMAC empty message:\n  got:  %x\n  want: %x", mac, expected)
	}
}

// TestCMAC_RFC4493_16ByteMessage tests AES-CMAC with a 16-byte message.
// Message: 6bc1bee22e409f96e93d7e117393172a
// Expected MAC: 070a16b46b4d4144f79bdd9dd04a287c
func TestCMAC_RFC4493_16ByteMessage(t *testing.T) {
	signer := NewCMACSigner(cmacTestKey)
	msg := mustDecodeHex("6bc1bee22e409f96e93d7e117393172a")
	expected := mustDecodeHex("070a16b46b4d4144f79bdd9dd04a287c")

	mac := signer.cmacMAC(msg)

	if !bytes.Equal(mac[:], expected) {
		t.Errorf("CMAC 16-byte message:\n  got:  %x\n  want: %x", mac, expected)
	}
}

// TestCMAC_RFC4493_40ByteMessage tests AES-CMAC with a 40-byte message.
// Message: 6bc1bee22e409f96e93d7e117393172a ae2d8a571e03ac9c9eb76fac45af8e51 30c81c46a35ce411
// Expected MAC: dfa66747de9ae63030ca32611497c827
func TestCMAC_RFC4493_40ByteMessage(t *testing.T) {
	signer := NewCMACSigner(cmacTestKey)
	msg := mustDecodeHex("6bc1bee22e409f96e93d7e117393172aae2d8a571e03ac9c9eb76fac45af8e5130c81c46a35ce411")
	expected := mustDecodeHex("dfa66747de9ae63030ca32611497c827")

	mac := signer.cmacMAC(msg)

	if !bytes.Equal(mac[:], expected) {
		t.Errorf("CMAC 40-byte message:\n  got:  %x\n  want: %x", mac, expected)
	}
}

// TestCMAC_RFC4493_64ByteMessage tests AES-CMAC with a 64-byte message.
// Message: 6bc1bee22e409f96e93d7e117393172a ae2d8a571e03ac9c9eb76fac45af8e51 30c81c46a35ce411 e5fbc1191a0a52ef f69f2445df4f9b17 ad2b417be66c3710
// Expected MAC: 51f0bebf7e3b9d92fc49741779363cfe
func TestCMAC_RFC4493_64ByteMessage(t *testing.T) {
	signer := NewCMACSigner(cmacTestKey)
	msg := mustDecodeHex("6bc1bee22e409f96e93d7e117393172aae2d8a571e03ac9c9eb76fac45af8e5130c81c46a35ce411e5fbc1191a0a52eff69f2445df4f9b17ad2b417be66c3710")
	expected := mustDecodeHex("51f0bebf7e3b9d92fc49741779363cfe")

	mac := signer.cmacMAC(msg)

	if !bytes.Equal(mac[:], expected) {
		t.Errorf("CMAC 64-byte message:\n  got:  %x\n  want: %x", mac, expected)
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
