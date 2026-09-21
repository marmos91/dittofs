package metadata

import (
	"context"
	"errors"
	"io"

	"github.com/marmos91/dittofs/pkg/block"
)

// Snapshotable is an optional capability that metadata store backends may
// implement to support share-level snapshot and restore. It is deliberately
// NOT embedded in MetadataStore so that protocol handlers and the runtime
// never depend on snapshot support existing.
//
// Call sites discover the capability via a type assertion:
//
//	if b, ok := store.(metadata.Snapshotable); ok {
//	    hashes, err := b.WriteSnapshot(ctx, w)
//	    ...
//	}
//
// WriteSnapshot writes engine-specific metadata into w and returns the set of
// block hashes referenced by the snapshot. The caller is responsible for
// placing GC holds on those hashes before the snapshot stream is considered
// durable.
//
// RestoreSnapshot reads a previously-written snapshot stream from r and rebuilds
// the metadata state. The destination store must be empty (no existing share
// data); otherwise ErrRestoreDestinationNotEmpty is returned.
//
// Consistency contract (issue #811): blocks are immutable and
// content-addressed, so the only source of metadata-vs-block skew is the
// dump and the returned hash set being read at different logical instants.
// Implementations MUST therefore capture BOTH the serialized metadata
// written to w AND the returned HashSet from a single consistent read-view
// — e.g. a single MVCC/REPEATABLE READ transaction (postgres), one managed
// read txn (badger), or a copy-on-read under the write lock (memory). The
// hash set MUST be derived from that same view (enumerating the captured
// files' block refs), never from a later live re-read. Writes may proceed
// concurrently; no global quiesce is required. The result is a true
// point-in-time image: a file present in the dump always has its blocks in
// the manifest, and vice versa. The ConcurrentWriter case in the storetest
// snapshot conformance suite enforces this.
type Snapshotable interface {
	// WriteSnapshot serializes all metadata into w and returns the set of
	// content-addressed block hashes referenced by the snapshot, both
	// captured from a single consistent read-view (see contract above).
	WriteSnapshot(ctx context.Context, w io.Writer) (*block.HashSet, error)

	// RestoreSnapshot reads a snapshot stream from r and rebuilds metadata state.
	// The store must be empty; returns ErrRestoreDestinationNotEmpty otherwise.
	RestoreSnapshot(ctx context.Context, r io.Reader) error
}

// SnapshotDegradation describes what a snapshot could not capture. A snapshot
// carrying one is a DEGRADED snapshot: its stream and its HashSet are both
// short by these entries, and it must never be presented as equivalent to a
// complete one.
//
// Keys identify the rows well enough to go and look at them. The list is
// bounded, so Entries — not len(Keys) — is the authoritative count.
type SnapshotDegradation struct {
	// Entries is how many rows were skipped. Always the full count.
	Entries int
	// Keys names the skipped rows, truncated to a bounded sample.
	Keys []string
	// KeysTruncated reports that Keys holds fewer than Entries names.
	KeysTruncated bool
}

// DegradableSnapshotter is an optional capability a Snapshotable backend may
// also implement: it can finish a snapshot over data it cannot fully read,
// provided it reports exactly what it left out.
//
// The split exists because the two callers want opposite things from the same
// corruption. An ordinary backup must refuse — a snapshot whose hash claim is
// silently short is worse than no snapshot. But restore takes a safety
// snapshot first, and refusing there blocks recovery from the very corruption
// that triggered the refusal, leaving the share disabled with no undo point. A
// degraded undo point beats none, as long as it is labelled.
//
// Discover it the same way Snapshotable itself is discovered:
//
//	if d, ok := store.(metadata.DegradableSnapshotter); ok {
//	    hashes, degraded, err := d.WriteSnapshotDegraded(ctx, w)
//	    ...
//	}
//
// A backend that cannot produce a partial read at all need not implement it;
// callers fall back to WriteSnapshot and get the refusal.
type DegradableSnapshotter interface {
	// WriteSnapshotDegraded behaves exactly like WriteSnapshot, except that an
	// entry it cannot decode is skipped and recorded rather than aborting the
	// snapshot. It returns a non-nil *SnapshotDegradation if and only if
	// something was skipped; a nil one means the snapshot is complete and is
	// byte-for-byte what WriteSnapshot would have produced.
	//
	// Every other failure still aborts, so this widens exactly one class.
	// The caller MUST record a non-nil degradation with the snapshot; ignoring
	// it reproduces the defect this exists to avoid.
	WriteSnapshotDegraded(ctx context.Context, w io.Writer) (*block.HashSet, *SnapshotDegradation, error)
}

// Snapshot/restore error sentinels. Callers detect these via errors.Is
// through any wrapping depth.
var (
	// ErrRestoreDestinationNotEmpty is returned by RestoreSnapshot when the target
	// store already contains data. The caller must provide an empty store.
	ErrRestoreDestinationNotEmpty = errors.New("metadata: restore destination is not empty")

	// ErrRestoreCorrupt is returned by RestoreSnapshot when the input stream
	// fails integrity checks (bad CRC, truncated envelope, malformed
	// engine payload).
	ErrRestoreCorrupt = errors.New("metadata: restore data is corrupt")

	// ErrSchemaVersionMismatch is returned by RestoreSnapshot when the snapshot
	// stream's schema version does not match the running engine's expected
	// version. The operator must upgrade or downgrade the server before
	// restoring.
	ErrSchemaVersionMismatch = errors.New("metadata: schema version mismatch")

	// ErrSnapshotAborted is returned by WriteSnapshot when the operation is
	// cancelled (context done) or otherwise cannot complete. Partial
	// output written to w must be discarded by the caller.
	ErrSnapshotAborted = errors.New("metadata: snapshot aborted")
)
