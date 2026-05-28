// Package encryption provides a per-remote BlockStore decorator that
// encrypts block payloads before they leave the host and decrypts them
// on the way back, preserving the BLAKE3-over-plaintext CAS key.
//
// The design is standard envelope encryption (matches AWS SSE-KMS
// MinIO+KES, HashiCorp Vault Transit)
//
//   - A master key is held by a [keyprovider.KeyProvider] (local key file
//     or external KMIP HSM). The master key never directly encrypts a
//     block.
//   - Per block, a fresh 32-byte block key is generated and used with an
//     AEAD to encrypt the payload. The block key is wrapped under the
//     master key (the wrapped bytes live in the frame header).
//   - On read: decode header → unwrap block key via the provider →
//     authenticated-decrypt the payload.
//
// The decorator should sit BELOW any compression decorator: compress on
// the way down, then encrypt; decrypt on the way up, then decompress.
// Encrypted bytes are incompressible, so reversing the order destroys
// compressibility.
//
// On-wire frame format (per block)
//
//	offset 0..4 magic 5 bytes "DFENC"
//	offset 5 version 1 byte 0x01
//	offset 6 aead algorithm 1 byte 1: AES-256-GCM, 2: ChaCha20-Poly1305, 3: XChaCha20-Poly1305
//	offset 7 wrap kind 1 byte 0x01 (keyprovider managed)
//	offset 8.. master-key-id uvarint length + bytes
//	offset .. wrapped block key uvarint length + bytes
//	offset .. nonce 1-byte length + bytes (12 for GCM/Poly1305, 24 for XChaCha20)
//	offset .. ciphertext + tag rest of the body
//
// Encryption is opt-in per remote via the BlockStoreConfig.Config JSON
//
//	{
//	  "encryption": {
//	"aead": "aes-256-gcm"
//	    "key": {
//	"kind": "local"
//	      "file": "/etc/dittofs/keys/share.key"
//	    }
//	  }
//	}
//
// The passphrase that unlocks the local key file is read from the
// DITTOFS_ENCRYPTION_PASSPHRASE environment variable.
//
// Absence of the encryption block means no wrapping (zero behavior
// change). AEAD defaults to AES-256-GCM when the encryption block is
// present without an explicit aead key.
package encryption
