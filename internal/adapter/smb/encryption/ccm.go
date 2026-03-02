// Vendored CCM (Counter with CBC-MAC) cipher.AEAD implementation.
//
// Based on github.com/pion/dtls/v2/pkg/crypto/ccm (MIT License).
// Original source: https://github.com/pion/dtls/tree/master/pkg/crypto/ccm
//
// Copyright 2018 Pion Contributors. All rights reserved.
// Use of this source code is governed by a MIT-style license.
//
// This implements RFC 3610: Counter with CBC-MAC (CCM) as a cipher.AEAD.
// It is vendored to avoid pulling in the full pion/dtls dependency tree
// for a single ~200 line file.
package encryption

import (
	"crypto/cipher"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"math"
)

// ccmAEAD implements cipher.AEAD for CCM mode.
type ccmAEAD struct {
	block     cipher.Block
	tagSize   int
	nonceSize int
}

// NewCCM creates a new CCM cipher.AEAD instance.
// block must be an AES cipher.Block. tagSize is the authentication tag size in bytes
// (must be 4, 6, 8, 10, 12, 14, or 16). nonceSize is the nonce size in bytes
// (must be between 7 and 13 inclusive).
func NewCCM(block cipher.Block, tagSize, nonceSize int) (cipher.AEAD, error) {
	if tagSize < 4 || tagSize > 16 || tagSize&1 != 0 {
		return nil, errors.New("ccm: invalid tag size")
	}
	if nonceSize < 7 || nonceSize > 13 {
		return nil, errors.New("ccm: invalid nonce size")
	}
	return &ccmAEAD{
		block:     block,
		tagSize:   tagSize,
		nonceSize: nonceSize,
	}, nil
}

func (c *ccmAEAD) NonceSize() int { return c.nonceSize }
func (c *ccmAEAD) Overhead() int  { return c.tagSize }

// Seal encrypts and authenticates plaintext with additional data and nonce,
// appending the result to dst and returning the updated slice.
func (c *ccmAEAD) Seal(dst, nonce, plaintext, additionalData []byte) []byte {
	if len(nonce) != c.nonceSize {
		panic("ccm: incorrect nonce length")
	}

	// Maximum message length for the given L value
	q := 15 - c.nonceSize // L value
	if uint64(len(plaintext)) > maxMessageLen(q) {
		panic("ccm: plaintext too large")
	}

	// Compute CBC-MAC tag
	tag := c.computeTag(nonce, plaintext, additionalData)

	// Encrypt with CTR mode
	ret, out := sliceForAppend(dst, len(plaintext)+c.tagSize)

	// Generate S_0 (for tag encryption) and S_1..S_n (for plaintext encryption)
	ctr := c.makeCounter(nonce, q)
	s0 := make([]byte, c.block.BlockSize())
	c.block.Encrypt(s0, ctr)

	// Encrypt plaintext using CTR mode starting from counter = 1
	incrementCounter(ctr, q)
	ctrStream := cipher.NewCTR(c.block, ctr)
	ctrStream.XORKeyStream(out[:len(plaintext)], plaintext)

	// Encrypt tag with S_0
	for i := 0; i < c.tagSize; i++ {
		out[len(plaintext)+i] = tag[i] ^ s0[i]
	}

	return ret
}

// Open decrypts and authenticates ciphertext with additional data and nonce,
// appending the result to dst and returning the updated slice.
func (c *ccmAEAD) Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error) {
	if len(nonce) != c.nonceSize {
		return nil, errors.New("ccm: incorrect nonce length")
	}
	if len(ciphertext) < c.tagSize {
		return nil, errors.New("ccm: ciphertext too short")
	}

	q := 15 - c.nonceSize

	// Separate ciphertext and encrypted tag
	ctLen := len(ciphertext) - c.tagSize
	ct := ciphertext[:ctLen]
	encTag := ciphertext[ctLen:]

	// Generate S_0 for tag decryption
	ctr := c.makeCounter(nonce, q)
	s0 := make([]byte, c.block.BlockSize())
	c.block.Encrypt(s0, ctr)

	// Decrypt tag
	tag := make([]byte, c.tagSize)
	for i := 0; i < c.tagSize; i++ {
		tag[i] = encTag[i] ^ s0[i]
	}

	// Decrypt ciphertext using CTR mode starting from counter = 1
	ret, out := sliceForAppend(dst, ctLen)
	incrementCounter(ctr, q)
	ctrStream := cipher.NewCTR(c.block, ctr)
	ctrStream.XORKeyStream(out, ct)

	// Recompute and verify tag
	expectedTag := c.computeTag(nonce, out, additionalData)
	if subtle.ConstantTimeCompare(tag, expectedTag[:c.tagSize]) != 1 {
		// Zero output to prevent leaking plaintext on authentication failure
		for i := range out {
			out[i] = 0
		}
		return nil, errors.New("ccm: message authentication failed")
	}

	return ret, nil
}

// computeTag computes the CBC-MAC authentication tag per RFC 3610 Section 2.2.
func (c *ccmAEAD) computeTag(nonce, plaintext, additionalData []byte) []byte {
	q := 15 - c.nonceSize

	// Format the first block B_0
	b0 := make([]byte, c.block.BlockSize())

	// Flags byte: (Adata ? 0x40 : 0) | (((t-2)/2) << 3) | (q-1)
	flags := byte(0)
	if len(additionalData) > 0 {
		flags |= 0x40
	}
	flags |= byte((c.tagSize-2)/2) << 3
	flags |= byte(q - 1)
	b0[0] = flags

	// Nonce
	copy(b0[1:1+c.nonceSize], nonce)

	// Message length in the remaining bytes
	msgLen := len(plaintext)
	for i := q; i > 0; i-- {
		b0[c.nonceSize+i] = byte(msgLen)
		msgLen >>= 8
	}

	// CBC-MAC over B_0
	mac := make([]byte, c.block.BlockSize())
	c.block.Encrypt(mac, b0)

	// Add additional data if present
	if len(additionalData) > 0 {
		c.cbcMACAdditionalData(mac, additionalData)
	}

	// Add plaintext blocks
	c.cbcMACBlocks(mac, plaintext)

	return mac[:c.tagSize]
}

// cbcMACAdditionalData processes the additional data per RFC 3610 Section 2.2.
func (c *ccmAEAD) cbcMACAdditionalData(mac, additionalData []byte) {
	blockSize := c.block.BlockSize()
	aLen := len(additionalData)

	// Encode length of additional data
	var lenBuf []byte
	switch {
	case aLen < 0xFF00:
		lenBuf = make([]byte, 2)
		binary.BigEndian.PutUint16(lenBuf, uint16(aLen))
	case aLen <= math.MaxUint32:
		lenBuf = make([]byte, 6)
		lenBuf[0] = 0xFF
		lenBuf[1] = 0xFE
		binary.BigEndian.PutUint32(lenBuf[2:], uint32(aLen))
	default:
		lenBuf = make([]byte, 10)
		lenBuf[0] = 0xFF
		lenBuf[1] = 0xFF
		binary.BigEndian.PutUint64(lenBuf[2:], uint64(aLen))
	}

	// Concatenate length encoding and additional data, pad to block boundary
	data := append(lenBuf, additionalData...)
	if rem := len(data) % blockSize; rem != 0 {
		data = append(data, make([]byte, blockSize-rem)...)
	}

	c.cbcMACBlocks(mac, data)
}

// cbcMACBlocks performs CBC-MAC over data blocks, XORing with running MAC state.
func (c *ccmAEAD) cbcMACBlocks(mac, data []byte) {
	blockSize := c.block.BlockSize()
	for i := 0; i < len(data); i += blockSize {
		end := i + blockSize
		if end > len(data) {
			// Pad the last block with zeros
			block := make([]byte, blockSize)
			copy(block, data[i:])
			xorBytes(mac, mac, block)
		} else {
			xorBytes(mac, mac, data[i:end])
		}
		c.block.Encrypt(mac, mac)
	}
}

// makeCounter creates the initial counter block A_0 per RFC 3610 Section 2.3.
func (c *ccmAEAD) makeCounter(nonce []byte, q int) []byte {
	ctr := make([]byte, c.block.BlockSize())
	ctr[0] = byte(q - 1) // Flags: q-1
	copy(ctr[1:1+c.nonceSize], nonce)
	// Counter value starts at 0 (for S_0)
	return ctr
}

// incrementCounter increments the q-byte counter in the counter block.
func incrementCounter(ctr []byte, q int) {
	for i := len(ctr) - 1; i >= len(ctr)-q; i-- {
		ctr[i]++
		if ctr[i] != 0 {
			break
		}
	}
}

// maxMessageLen returns the maximum message length for the given L value.
func maxMessageLen(q int) uint64 {
	// Maximum is 2^(8*q) - 1, capped to avoid overflow
	if q >= 8 {
		return math.MaxUint64
	}
	return (1 << (8 * uint(q))) - 1
}

// xorBytes XORs src1 and src2 into dst. All slices must be the same length.
func xorBytes(dst, src1, src2 []byte) {
	for i := range dst {
		dst[i] = src1[i] ^ src2[i]
	}
}

// sliceForAppend is a helper that takes a slice and a requested size, returning
// the slice with capacity for the requested bytes and the sub-slice for writing.
func sliceForAppend(in []byte, n int) (head, tail []byte) {
	if total := len(in) + n; cap(in) >= total {
		head = in[:total]
	} else {
		head = make([]byte, total)
		copy(head, in)
	}
	tail = head[len(in):]
	return
}
