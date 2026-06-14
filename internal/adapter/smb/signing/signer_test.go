package signing

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

func TestNewSigner_Dispatch(t *testing.T) {
	key := make([]byte, 16)
	for i := range key {
		key[i] = byte(i + 1)
	}

	tests := []struct {
		name       string
		dialect    types.Dialect
		signingAlg uint16
		explicit   bool
		wantType   string
	}{
		{
			name:       "SMB 2.0.2 returns HMACSigner",
			dialect:    types.Dialect0202,
			signingAlg: 0,
			wantType:   "*signing.HMACSigner",
		},
		{
			name:       "SMB 2.1 returns HMACSigner",
			dialect:    types.Dialect0210,
			signingAlg: 0,
			wantType:   "*signing.HMACSigner",
		},
		{
			name:       "SMB 3.0 with AES-CMAC returns CMACSigner",
			dialect:    types.Dialect0300,
			signingAlg: SigningAlgAESCMAC,
			wantType:   "*signing.CMACSigner",
		},
		{
			name:       "SMB 3.0.2 with default returns CMACSigner",
			dialect:    types.Dialect0302,
			signingAlg: 0,
			wantType:   "*signing.CMACSigner",
		},
		{
			name:       "SMB 3.1.1 with AES-GMAC returns GMACSigner",
			dialect:    types.Dialect0311,
			signingAlg: SigningAlgAESGMAC,
			wantType:   "*signing.GMACSigner",
		},
		{
			name:       "SMB 3.1.1 with AES-CMAC returns CMACSigner",
			dialect:    types.Dialect0311,
			signingAlg: SigningAlgAESCMAC,
			wantType:   "*signing.CMACSigner",
		},
		{
			name:       "SMB 3.1.1 with explicit HMAC-SHA256 returns HMACSigner",
			dialect:    types.Dialect0311,
			signingAlg: SigningAlgHMACSHA256,
			explicit:   true,
			wantType:   "*signing.HMACSigner",
		},
		{
			// Regression: HMAC-SHA256 has wire value 0x0000, which is
			// indistinguishable from a default-zero placeholder. Without
			// an explicit-selection signal, a 3.1.1 session that never
			// negotiated SIGNING_CAPABILITIES (or any other default-zero
			// plumbing) must fall through to CMAC.
			name:       "SMB 3.1.1 with implicit zero signingAlg returns CMACSigner",
			dialect:    types.Dialect0311,
			signingAlg: SigningAlgHMACSHA256,
			explicit:   false,
			wantType:   "*signing.CMACSigner",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			signer := NewSigner(tt.dialect, tt.signingAlg, tt.explicit, key)
			if signer == nil {
				t.Fatal("NewSigner returned nil")
			}

			// Type check via type assertion
			var typeName string
			switch signer.(type) {
			case *HMACSigner:
				typeName = "*signing.HMACSigner"
			case *CMACSigner:
				typeName = "*signing.CMACSigner"
			case *GMACSigner:
				typeName = "*signing.GMACSigner"
			default:
				typeName = "unknown"
			}

			if typeName != tt.wantType {
				t.Errorf("NewSigner(%v, 0x%04x, explicit=%v) = %s, want %s",
					tt.dialect, tt.signingAlg, tt.explicit, typeName, tt.wantType)
			}
		})
	}
}

// signingMessage builds a deterministic SMB2 message with a non-zero
// signature field, so tests can assert Sign neither mutates the caller's
// buffer nor depends on the field already being zeroed.
func signingMessage() []byte {
	msg := make([]byte, SMB2HeaderSize+40)
	for i := range msg {
		msg[i] = byte(i*7 + 3)
	}
	msg[0], msg[1], msg[2], msg[3] = 0xFE, 'S', 'M', 'B'
	return msg
}

func eachSigner(key []byte) []struct {
	name   string
	signer Signer
} {
	return []struct {
		name   string
		signer Signer
	}{
		{"HMAC", NewHMACSigner(key)},
		{"CMAC", NewCMACSigner(key)},
		{"GMAC", NewGMACSigner(key)},
	}
}

// TestSign_DoesNotMutateBuffer guards the public Sign contract: it must leave
// the caller's buffer byte-identical on return (including the original,
// non-zero signature field). A regression here corrupts the outbound PDU.
func TestSign_DoesNotMutateBuffer(t *testing.T) {
	key := make([]byte, 16)
	for i := range key {
		key[i] = byte(i + 1)
	}

	for _, tc := range eachSigner(key) {
		t.Run(tc.name, func(t *testing.T) {
			msg := signingMessage()
			orig := make([]byte, len(msg))
			copy(orig, msg)

			_ = tc.signer.Sign(msg)

			if !bytes.Equal(msg, orig) {
				t.Errorf("%s.Sign mutated the caller's buffer", tc.name)
			}
		})
	}
}

// TestSignInPlace_MatchesSign is the golden cross-check that the allocation-free
// outbound path produces byte-identical signatures to the copying Sign path.
// SignInPlace assumes the 16-byte signature field is already zeroed, which is
// exactly the precondition SignMessage establishes before calling it.
func TestSignInPlace_MatchesSign(t *testing.T) {
	key := make([]byte, 16)
	for i := range key {
		key[i] = byte(i + 1)
	}

	for _, tc := range eachSigner(key) {
		t.Run(tc.name, func(t *testing.T) {
			msg := signingMessage()

			want := tc.signer.Sign(msg)

			zeroed := make([]byte, len(msg))
			copy(zeroed, msg)
			zeroSignatureField(zeroed)
			got := tc.signer.SignInPlace(zeroed)

			if want != got {
				t.Errorf("%s: SignInPlace=%x != Sign=%x", tc.name, got, want)
			}
		})
	}
}

// TestSignMessage_SetsFlagAndSignature verifies the standalone SignMessage helper.
func TestSignMessage_SetsFlagAndSignature(t *testing.T) {
	key := make([]byte, 16)
	for i := range key {
		key[i] = byte(i + 1)
	}
	signer := NewHMACSigner(key)

	message := make([]byte, SMB2HeaderSize+20)
	message[0], message[1], message[2], message[3] = 0xFE, 'S', 'M', 'B'

	SignMessage(signer, message)

	// Check SMB2_FLAGS_SIGNED is set
	flags := uint32(message[16]) | uint32(message[17])<<8 | uint32(message[18])<<16 | uint32(message[19])<<24
	if flags&0x00000008 == 0 {
		t.Error("SignMessage did not set SMB2_FLAGS_SIGNED flag")
	}

	// Check signature is non-zero
	allZero := true
	for i := SignatureOffset; i < SignatureOffset+SignatureSize; i++ {
		if message[i] != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		t.Error("SignMessage did not write signature")
	}

	// Verify should pass
	if !signer.Verify(message) {
		t.Error("Verify failed after SignMessage")
	}
}
