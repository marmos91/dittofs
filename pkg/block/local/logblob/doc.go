// Package logblob implements a directory-backed sequence of raw log blobs.
//
// A [Manager] owns a directory and provides [Manager.Append] / [Manager.ReadAt]
// primitives over a sequence of bounded raw files ("blobs"). One blob is active
// (writable at its tail); older blobs are sealed and read-only. Blob IDs are
// monotonically increasing, zero-padded decimal strings that sort
// lexicographically in creation order and remain stable across [Manager]
// close/reopen.
//
// Append is mutex-serialized; ReadAt uses pread(2) (via [os.File.ReadAt]) and
// is safe to call concurrently with active Appends — reads and writes operate
// at explicit byte offsets and never share a file cursor.
//
// Bytes are stored raw: no framing, no checksum, no compression. The caller
// controls fsync cadence via the explicit [Manager.Sync] method.
//
// This package is an internal primitive for #1493 PR2 Task 5. It is wired into
// higher-level stores (FSStore, engine) in PR3 — do NOT import it from FSStore
// in this iteration.
package logblob
