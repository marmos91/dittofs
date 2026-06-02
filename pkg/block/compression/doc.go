// Package compression provides a per-remote BlockStore decorator that
// compresses block payloads before they leave the host and decompresses
// them on the way back, preserving the BLAKE3-over-plaintext CAS key.
//
// The decorator wraps remote.RemoteStore and is installed once per remote
// in the controlplane share service. Engine, cache, GC, and metadata
// stores see only plaintext bytes; compression is opaque above and below.
//
// On-wire format (per block when compression actually shrinks the body)
//
//	offset 0..4 magic 5 bytes "DFCMP"
//	offset 5 algo 1 byte 1=zstd, 2=lz4
//	offset 6.. orig_size uvarint (1-10 bytes)
//	offset N.. body compressed bytes
//
// If a block does not compress (len(compressed) >= len(plaintext)), the
// decorator writes the raw plaintext with no header — the Get path
// detects framed vs raw by checking the 5-byte magic prefix.
//
// Compression is opt-in per remote via the BlockStoreConfig.Config JSON
//
//	{ "compression": { "algo": "zstd" } }
//
// Absence of the compression block means no wrapping (zero behavior
// change). Algorithm defaults to zstd when the block is present without
// an explicit algo key.
//
// # Composition order
//
// When the encryption decorator is also enabled for a remote, compression
// is the OUTERMOST wrapper and encryption is INNERMOST, so the on-Put
// data flow is
//
//	caller plaintext
//	  → compression.Decorator (compress)
//	  → encryption.EncryptedRemote (encrypt compressed bytes)
//	  → inner remote.RemoteStore
//
// Get inverts the order: fetch ciphertext, decrypt, then decompress.
//
// Rationale for compressing first: AEAD output has near-maximum entropy,
// so attempting to compress ciphertext yields a ratio of ~1.0 and burns
// CPU for nothing. Compressing plaintext is the only ordering that
// preserves any space saving.
//
// The CAS key is BLAKE3 over the PLAINTEXT bytes for both decorators —
// neither compression framing nor per-block encryption keys influence
// the hash. Dedup therefore works across remotes that differ in
// compression algorithm, encryption key, or AEAD choice: identical
// plaintexts always map to the same content hash. Get path verification
// hashes the recovered plaintext (after decrypt + decompress) against
// the requested hash; see [Decorator.ReadBlockVerified] and
// [encryption.EncryptedRemote.ReadBlockVerified].
//
// Composition is fixed in the controlplane share service — there is no
// runtime toggle to flip the order. The order is established once at
// remote-store construction and immutable for the lifetime of the
// remote. The example wiring snippet lives in the encryption package
// doc, since that is where both decorators are visible together.
package compression
