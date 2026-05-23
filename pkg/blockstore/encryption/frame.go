package encryption

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// FrameMagic is the 5-byte prefix every encrypted block starts with.
var FrameMagic = [5]byte{'D', 'F', 'E', 'N', 'C'}

// FrameVersion is the on-wire format version this build writes. Bumped
// when the layout below changes in a non-backwards-compatible way.
const FrameVersion byte = 1

// frameHeaderFixedSize is the byte count of magic + version + aead +
// wrap-kind. The variable-length fields (master-key-id, wrapped block
// key, nonce) follow.
const frameHeaderFixedSize = len(FrameMagic) + 3

const maxVarint = binary.MaxVarintLen64

// MaxWrappedBlockKeySize caps the wrapped block key length a frame may
// declare. Guards against absurd preallocations from a corrupt header.
// 4 KiB easily covers AES-GCM wraps (~60 B) plus future RSA-OAEP wraps
// (~512 B for RSA-4096).
const MaxWrappedBlockKeySize = 4096

// MaxMasterKeyIDSize caps the master-key identifier length a frame may
// declare. KMIP UUIDs are 36 ASCII bytes; 256 is generous.
const MaxMasterKeyIDSize = 256

// MaxNonceSize caps the nonce length a frame may declare. AES-GCM and
// Poly1305 use 12 bytes; XChaCha20 uses 24; future algorithms are
// unlikely to exceed 64.
const MaxNonceSize = 64

// wrapKindKeyProvider identifies the wrap algorithm encoded in the
// frame. Today only one value is in use (the keyprovider-managed wrap)
// but the byte is reserved so future providers can negotiate without a
// magic bump.
const wrapKindKeyProvider byte = 1

// aeadTagSize is the AEAD authentication-tag length in bytes. All three
// supported algorithms (AES-256-GCM, ChaCha20-Poly1305, XChaCha20-
// Poly1305) emit a 16-byte tag, so plaintext_size = wire_size -
// header_size - aeadTagSize without per-algo branching.
const aeadTagSize = 16

// maxFrameHeaderSize is the worst-case length of the fixed + variable
// header bytes (everything before the ciphertext+tag). Used to bound a
// range-GET probe that recovers the plaintext size without decrypting
// the payload — see frameHeaderSize.
const maxFrameHeaderSize = frameHeaderFixedSize +
	maxVarint + MaxMasterKeyIDSize +
	maxVarint + MaxWrappedBlockKeySize +
	1 + MaxNonceSize

// frameHeaderSize parses just enough of a frame prefix to return the
// byte length of the header (everything before the ciphertext). The
// input must begin with the DFENC magic; framed=false and err=nil mean
// the input is unframed.
func frameHeaderSize(b []byte) (headerLen int, framed bool, err error) {
	view, framed, err := tryDecodeFrame(b)
	if !framed || err != nil {
		return 0, framed, err
	}
	// view.ciphertext aliases the input slice; its offset from b's start
	// is the header length. cap(b) - cap(view.ciphertext) is robust
	// regardless of how much trailing data we passed to tryDecodeFrame.
	return len(b) - len(view.ciphertext), true, nil
}

// encodeFrame builds the wire form for an encrypted block:
//
//	[magic | version | aead | wrap | uvarint(len(masterKeyID)) | masterKeyID
//	 | uvarint(len(wrappedKey)) | wrappedKey
//	 | byte(len(nonce)) | nonce
//	 | ciphertext+tag]
func encodeFrame(aead AEAD, masterKeyID string, wrappedKey, nonce, ciphertext []byte) ([]byte, error) {
	if len(masterKeyID) > MaxMasterKeyIDSize {
		return nil, fmt.Errorf("encryption: master_key_id length %d exceeds cap %d", len(masterKeyID), MaxMasterKeyIDSize)
	}
	if len(wrappedKey) > MaxWrappedBlockKeySize {
		return nil, fmt.Errorf("encryption: wrapped block key length %d exceeds cap %d", len(wrappedKey), MaxWrappedBlockKeySize)
	}
	if len(nonce) == 0 || len(nonce) > MaxNonceSize {
		return nil, fmt.Errorf("encryption: nonce length %d out of range (1..%d)", len(nonce), MaxNonceSize)
	}

	headerCap := frameHeaderFixedSize + maxVarint + len(masterKeyID) + maxVarint + len(wrappedKey) + 1 + len(nonce) + len(ciphertext)
	out := make([]byte, 0, headerCap)
	out = append(out, FrameMagic[:]...)
	out = append(out, FrameVersion, byte(aead), wrapKindKeyProvider)

	var sizeBuf [maxVarint]byte
	n := binary.PutUvarint(sizeBuf[:], uint64(len(masterKeyID)))
	out = append(out, sizeBuf[:n]...)
	out = append(out, masterKeyID...)

	n = binary.PutUvarint(sizeBuf[:], uint64(len(wrappedKey)))
	out = append(out, sizeBuf[:n]...)
	out = append(out, wrappedKey...)

	out = append(out, byte(len(nonce)))
	out = append(out, nonce...)
	out = append(out, ciphertext...)
	return out, nil
}

func hasFrameMagic(b []byte) bool {
	return len(b) >= frameHeaderFixedSize && bytes.Equal(b[:len(FrameMagic)], FrameMagic[:])
}

// frameView is the decoded view of a wire frame. Slices alias the input
// buffer — callers MUST NOT retain them past the lifetime of the
// underlying bytes.
type frameView struct {
	aead        AEAD
	masterKeyID string
	wrappedKey  []byte
	nonce       []byte
	ciphertext  []byte
}

// tryDecodeFrame parses b and returns the framed-payload view. When b
// does not carry the DFENC magic, framed is false and view is zero-
// valued — callers treat b as unencrypted bytes in that case.
//
// When the magic is present but the rest of the header is malformed, an
// error wrapping ErrEncryptedFrameCorrupt or ErrUnsupportedAEAD is
// returned with framed=true.
func tryDecodeFrame(b []byte) (view frameView, framed bool, err error) {
	if !hasFrameMagic(b) {
		return frameView{}, false, nil
	}
	if b[len(FrameMagic)] != FrameVersion {
		return frameView{}, true, fmt.Errorf("%w: unsupported version 0x%02x", ErrEncryptedFrameCorrupt, b[len(FrameMagic)])
	}
	aeadByte := b[len(FrameMagic)+1]
	switch AEAD(aeadByte) {
	case AEADAES256GCM, AEADChaCha20Poly1305, AEADXChaCha20Poly1305:
		view.aead = AEAD(aeadByte)
	default:
		return frameView{}, true, fmt.Errorf("%w: byte 0x%02x", ErrUnsupportedAEAD, aeadByte)
	}
	if wrap := b[len(FrameMagic)+2]; wrap != wrapKindKeyProvider {
		return frameView{}, true, fmt.Errorf("%w: unknown wrap byte 0x%02x", ErrEncryptedFrameCorrupt, wrap)
	}

	rest := b[frameHeaderFixedSize:]
	idLen, n := binary.Uvarint(rest)
	if n <= 0 {
		return frameView{}, true, fmt.Errorf("%w: bad master_key_id uvarint", ErrEncryptedFrameCorrupt)
	}
	if idLen > MaxMasterKeyIDSize {
		return frameView{}, true, fmt.Errorf("%w: master_key_id length %d exceeds cap %d", ErrEncryptedFrameCorrupt, idLen, MaxMasterKeyIDSize)
	}
	rest = rest[n:]
	if uint64(len(rest)) < idLen {
		return frameView{}, true, fmt.Errorf("%w: master_key_id truncated", ErrEncryptedFrameCorrupt)
	}
	view.masterKeyID = string(rest[:idLen])
	rest = rest[idLen:]

	wkLen, n := binary.Uvarint(rest)
	if n <= 0 {
		return frameView{}, true, fmt.Errorf("%w: bad wrapped_key uvarint", ErrEncryptedFrameCorrupt)
	}
	if wkLen > MaxWrappedBlockKeySize {
		return frameView{}, true, fmt.Errorf("%w: wrapped_key length %d exceeds cap %d", ErrEncryptedFrameCorrupt, wkLen, MaxWrappedBlockKeySize)
	}
	rest = rest[n:]
	if uint64(len(rest)) < wkLen {
		return frameView{}, true, fmt.Errorf("%w: wrapped_key truncated", ErrEncryptedFrameCorrupt)
	}
	view.wrappedKey = rest[:wkLen]
	rest = rest[wkLen:]

	if len(rest) < 1 {
		return frameView{}, true, fmt.Errorf("%w: missing nonce length", ErrEncryptedFrameCorrupt)
	}
	nonceLen := int(rest[0])
	if nonceLen == 0 || nonceLen > MaxNonceSize {
		return frameView{}, true, fmt.Errorf("%w: nonce length %d out of range", ErrEncryptedFrameCorrupt, nonceLen)
	}
	rest = rest[1:]
	if len(rest) < nonceLen {
		return frameView{}, true, fmt.Errorf("%w: nonce truncated", ErrEncryptedFrameCorrupt)
	}
	view.nonce = rest[:nonceLen]
	view.ciphertext = rest[nonceLen:]
	if len(view.ciphertext) == 0 {
		return frameView{}, true, fmt.Errorf("%w: empty ciphertext", ErrEncryptedFrameCorrupt)
	}
	return view, true, nil
}
