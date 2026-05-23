// Package compression provides a per-remote BlockStore decorator that
// compresses block payloads before they leave the host and decompresses
// them on the way back, preserving the BLAKE3-over-plaintext CAS key.
//
// The decorator wraps remote.RemoteStore and is installed once per remote
// in the controlplane share service. Engine, cache, GC, and metadata
// stores see only plaintext bytes; compression is opaque above and below.
//
// On-wire format (per block when compression actually shrinks the body):
//
//	offset 0..4   magic     5 bytes  "DFCMP"
//	offset 5      algo      1 byte   1=zstd, 2=lz4
//	offset 6..    orig_size uvarint  (1-10 bytes)
//	offset N..    body               compressed bytes
//
// If a block does not compress (len(compressed) >= len(plaintext)), the
// decorator writes the raw plaintext with no header — the Get path
// detects framed vs raw by checking the 5-byte magic prefix.
//
// Compression is opt-in per remote via the BlockStoreConfig.Config JSON:
//
//	{ "compression": { "algo": "zstd" } }
//
// Absence of the compression block means no wrapping (zero behavior
// change). Algorithm defaults to zstd when the block is present without
// an explicit algo key.
package compression
