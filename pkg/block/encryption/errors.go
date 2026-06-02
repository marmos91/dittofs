package encryption

import "errors"

var (
	// ErrUnsupportedAEAD is returned when a frame header or policy
	// specifies an AEAD algorithm byte / string this build does not
	// recognise.
	ErrUnsupportedAEAD = errors.New("encryption: unsupported aead algorithm")

	// ErrEncryptedFrameCorrupt is returned when a wire payload begins
	// with the DFENC magic but the rest of the header (version, algos
	// uvarint lengths, nonce length) fails to parse.
	ErrEncryptedFrameCorrupt = errors.New("encryption: corrupt frame header")

	// ErrDecryptAuth is returned when AEAD authenticated decryption fails
	// (tamper, wrong key, or corruption).
	ErrDecryptAuth = errors.New("encryption: authenticated decryption failed")

	// ErrCiphertextWithoutFrame is returned when a stored block lacks the
	// DFENC magic on Get. Encrypted shares only ever produce framed
	// blocks; reading an unframed block means the upstream store was
	// tampered with or the share's encryption policy was added after the
	// block was written.
	ErrCiphertextWithoutFrame = errors.New("encryption: stored block is not framed")
)
