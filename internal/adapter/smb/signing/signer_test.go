package signing

import (
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
			name:       "SMB 3.1.1 without GMAC negotiation returns CMACSigner",
			dialect:    types.Dialect0311,
			signingAlg: 0,
			wantType:   "*signing.CMACSigner",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			signer := NewSigner(tt.dialect, tt.signingAlg, key)
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
				t.Errorf("NewSigner(%v, 0x%04x) = %s, want %s",
					tt.dialect, tt.signingAlg, typeName, tt.wantType)
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
