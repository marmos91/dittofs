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
//
// # Composition order
//
// When the compression decorator is also enabled for a remote, encryption
// is the INNERMOST wrapper and compression is OUTERMOST, so the on-Put
// data flow is
//
//	caller plaintext
//	  → compression.Decorator (compress)
//	  → encryption.EncryptedRemote (encrypt compressed bytes)
//	  → inner remote.RemoteStore
//
// Get inverts the order: fetch ciphertext, decrypt, then decompress.
// Encryption operates on whatever bytes the compression layer hands it —
// the compressed body when the block was compressible, the raw
// plaintext otherwise (the compression layer skips its frame when the
// body would not shrink). EncryptedRemote does not know or care which
// shape it sees; it simply encrypts the bytes presented on Put.
//
// Rationale: AEAD output has near-maximum entropy, so compressing
// ciphertext yields a ratio of ~1.0 and wastes CPU. Compressing first
// is the only ordering that preserves space savings.
//
// The CAS key is BLAKE3 over the PLAINTEXT bytes — not over the
// compressed body, not over the ciphertext. Both decorators preserve
// that invariant: compression keeps the hash key untouched on the way
// down, and encryption binds the plaintext hash into the AEAD's
// additional-authenticated-data (the hash[:] argument to Seal / Open in
// [EncryptedRemote.Put] and the package-internal decrypt path). A swapped block
// at the inner store fails authentication on Get because its declared
// hash will not match the AAD bound at Put time. Dedup works across
// remotes with different keys, AEADs, or compression policies because
// identical plaintexts always hash to the same content key.
//
// Per-Put cryptographic material
//
//   - Block key: 32 random bytes from crypto/rand, fresh per Put,
//     wrapped under the share's master key by the configured
//     [keyprovider.KeyProvider] and stored in the frame header.
//   - Nonce: AEAD-specific length from crypto/rand, fresh per Put,
//     stored alongside the ciphertext (12 bytes for AES-256-GCM and
//     ChaCha20-Poly1305; 24 bytes for XChaCha20-Poly1305). XChaCha's
//     larger nonce is safe to draw at random at very high block counts;
//     the 12-byte variants assume a per-share block budget well inside
//     the birthday bound for random nonces.
//
// Composition is fixed in the controlplane share service — there is no
// runtime toggle to flip the order. The order is established once at
// remote-store construction and immutable for the lifetime of the
// remote.
//
// Example wiring (showing the canonical composition)
//
//	// inner is any concrete remote.RemoteStore (S3, on-disk, …).
//	encrypted, err := encryption.NewRemote(inner, encPolicy, keyProvider)
//	if err != nil {
//	    return nil, err
//	}
//	compressed, err := compression.NewRemote(encrypted, compPolicy)
//	if err != nil {
//	    _ = encrypted.Close()
//	    return nil, err
//	}
//	// Hand `compressed` to the engine as the BlockStore for the share.
//	// On Put the engine sees only plaintext; bytes are compressed first,
//	// then encrypted, then handed to inner.
package encryption
