// Package snapshot owns the on-disk hash manifest format used by the
// snapshot lifecycle in pkg/controlplane/runtime. A manifest is the
// authoritative list of block.ContentHash values referenced by a
// share at the moment a snapshot was taken; the snapshot orchestrator
// and the GC hold provider both read manifests through ReadManifest,
// and the orchestrator materializes them through WriteManifestAtomic.
//
// # Wire format
//
// The manifest is a plain-text file. There is no header, no footer, no
// comments, and no blank lines.
//
//   - ASCII only.
//   - One hex-encoded block.ContentHash per line, exactly 64
//     lowercase hex characters per line.
//   - Lines terminated by '\n' (LF). Readers tolerate CRLF on input;
//     writers always emit LF.
//   - Lines are sorted in ascending byte order — the order produced by
//     HashSet.Sorted (bytes.Compare). Writers consume that order
//     verbatim; readers do not depend on it (HashSet is unordered).
//
// The canonical path is <shareDataDir>/snapshots/<id>/manifest.hashes
// (see models.Snapshot.ManifestPath).
//
// # Atomic write contract
//
// WriteManifestAtomic uses a temp-file + fsync + rename sequence so that
// a reader either sees the previous manifest at the canonical path or
// the new, complete manifest — never a partially-written file. The temp
// file lives in the same directory as the destination so that the
// rename is a same-filesystem atomic operation.
//
// Parent-directory fsync is intentionally not performed here; if power-
// loss durability of the rename itself becomes load-bearing it belongs
// in the orchestrator that calls WriteManifestAtomic.
package snapshot
