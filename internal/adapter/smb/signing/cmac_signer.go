package signing

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/subtle"
)

// cmacRb is the constant Rb for 128-bit blocks per RFC 4493.
const cmacRb = 0x87

// CMACSigner implements the Signer interface using AES-128-CMAC per RFC 4493.
// This is used for SMB 3.x sessions.
type CMACSigner struct {
	key   [KeySize]byte
	block cipher.Block
	k1    [16]byte // Subkey K1
	k2    [16]byte // Subkey K2
}

// NewCMACSigner creates a CMACSigner from a signing key.
// Eagerly computes subkeys K1, K2 per RFC 4493 Section 2.3.
// Returns nil if the key is empty.
func NewCMACSigner(key []byte) *CMACSigner {
	if len(key) == 0 {
		return nil
	}

	s := &CMACSigner{}
	if len(key) >= KeySize {
		copy(s.key[:], key[:KeySize])
	} else {
		copy(s.key[:], key)
	}

	block, err := aes.NewCipher(s.key[:])
	if err != nil {
		return nil
	}
	s.block = block

	// Generate subkeys
	s.k1, s.k2 = generateSubkeys(block)

	return s
}

// generateSubkeys computes K1 and K2 per RFC 4493 Section 2.3.
//
//	Step 1: L = AES-Encrypt(K, 0^128)
//	Step 2: if MSB(L) == 0 then K1 = L << 1
//	         else K1 = (L << 1) XOR Rb
//	Step 3: if MSB(K1) == 0 then K2 = K1 << 1
//	         else K2 = (K1 << 1) XOR Rb
func generateSubkeys(block cipher.Block) (k1, k2 [16]byte) {
	// L = AES(K, 0^128)
	var zero [16]byte
	var L [16]byte
	block.Encrypt(L[:], zero[:])

	// K1 = shiftLeft(L); if MSB(L)==1 then K1 ^= Rb
	shiftLeft(L[:], k1[:])
	if L[0]&0x80 != 0 {
		k1[15] ^= cmacRb
	}

	// K2 = shiftLeft(K1); if MSB(K1)==1 then K2 ^= Rb
	shiftLeft(k1[:], k2[:])
	if k1[0]&0x80 != 0 {
		k2[15] ^= cmacRb
	}

	return
}

// shiftLeft performs a 1-bit left shift on a byte slice.
func shiftLeft(src, dst []byte) {
	var carry byte
	for i := len(src) - 1; i >= 0; i-- {
		dst[i] = (src[i] << 1) | carry
		carry = (src[i] >> 7) & 1
	}
}

// cmacMAC computes the raw AES-CMAC over the given data.
// This is the pure RFC 4493 algorithm (Section 2.4) without SMB2 header handling.
func (s *CMACSigner) cmacMAC(data []byte) [16]byte {
	n := len(data) / 16
	lastBlockComplete := false

	if n == 0 {
		n = 1
		lastBlockComplete = false
	} else if len(data)%16 == 0 {
		lastBlockComplete = true
	} else {
		n++ // one more block for the partial last block
	}

	// Prepare the last block
	var lastBlock [16]byte
	if lastBlockComplete {
		// Last block is complete: XOR with K1
		copy(lastBlock[:], data[(n-1)*16:])
		for i := 0; i < 16; i++ {
			lastBlock[i] ^= s.k1[i]
		}
	} else {
		// Last block is incomplete: pad and XOR with K2
		remaining := len(data) - (n-1)*16
		if remaining > 0 {
			copy(lastBlock[:remaining], data[(n-1)*16:])
		}
		lastBlock[remaining] = 0x80 // padding: 10*
		for i := 0; i < 16; i++ {
			lastBlock[i] ^= s.k2[i]
		}
	}

	// CBC-MAC over all blocks
	var x [16]byte // X starts as zero (IV = 0)
	for i := 0; i < n-1; i++ {
		// Y = X XOR M_i
		var y [16]byte
		for j := 0; j < 16; j++ {
			y[j] = x[j] ^ data[i*16+j]
		}
		// X = AES(K, Y)
		s.block.Encrypt(x[:], y[:])
	}

	// Last block: Y = X XOR lastBlock, then AES
	var y [16]byte
	for i := 0; i < 16; i++ {
		y[i] = x[i] ^ lastBlock[i]
	}
	var mac [16]byte
	s.block.Encrypt(mac[:], y[:])

	return mac
}

// Sign computes the AES-CMAC signature for an SMB2 message.
// The signature field (bytes 48-63) is zeroed before computation.
func (s *CMACSigner) Sign(message []byte) [SignatureSize]byte {
	var sig [SignatureSize]byte

	if len(message) < SMB2HeaderSize {
		return sig
	}

	// Create a copy with zeroed signature field
	msgCopy := make([]byte, len(message))
	copy(msgCopy, message)
	for i := SignatureOffset; i < SignatureOffset+SignatureSize; i++ {
		msgCopy[i] = 0
	}

	sig = s.cmacMAC(msgCopy)
	return sig
}

// Verify checks if the message signature is valid using constant-time comparison.
func (s *CMACSigner) Verify(message []byte) bool {
	if len(message) < SMB2HeaderSize {
		return false
	}

	// Extract the provided signature
	var providedSig [SignatureSize]byte
	copy(providedSig[:], message[SignatureOffset:SignatureOffset+SignatureSize])

	// Compute expected signature
	expectedSig := s.Sign(message)

	// Constant-time comparison
	return subtle.ConstantTimeCompare(providedSig[:], expectedSig[:]) == 1
}
