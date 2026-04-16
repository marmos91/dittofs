package backup

import (
	"context"
	"errors"
	"io"
)

// Backupable is the capability interface opted into by metadata stores that
// support streaming backup and restore.
//
// Capability is checked via Go type assertion at call sites:
//
//	if b, ok := store.(Backupable); ok {
//	    ids, err := b.Backup(ctx, w)
//	    ...
//	}
//
// Stores that cannot support backup/restore (for example, future read-only or
// virtual stores) simply do not implement the interface; callers surface
// ErrBackupUnsupported to operators (ENG-04). No runtime registry exists —
// the binding is compile-time.
//
// Implementations are provided in Phase 2 (memory, badger, postgres). This
// package only defines the contract.
type Backupable interface {
	// Backup streams a consistent snapshot of the store to w. The returned
	// PayloadIDSet records every block PayloadID referenced by the snapshot
	// at the moment of capture; consumers place a GC hold on the referenced
	// payloads (SAFETY-01) until the backup is durably committed.
	Backup(ctx context.Context, w io.Writer) (PayloadIDSet, error)

	// Restore reloads the store from r. The caller MUST guarantee the store
	// is drained (no active shares) before invoking Restore; implementations
	// are not required to enforce this.
	Restore(ctx context.Context, r io.Reader) error
}

// PayloadIDSet is the set of block PayloadIDs referenced by a snapshot.
// Used by the block-GC hold path (SAFETY-01).
type PayloadIDSet map[string]struct{}

// NewPayloadIDSet constructs an empty, non-nil PayloadIDSet ready for Add.
func NewPayloadIDSet() PayloadIDSet {
	return make(PayloadIDSet)
}

// Add inserts id into the set. Calling Add on a nil set panics — use
// NewPayloadIDSet to construct a writable instance.
func (s PayloadIDSet) Add(id string) { s[id] = struct{}{} }

// Contains reports whether id is present. Safe on a nil set (returns false).
func (s PayloadIDSet) Contains(id string) bool {
	_, ok := s[id]
	return ok
}

// Len returns the number of distinct IDs. Safe on a nil set (returns 0).
func (s PayloadIDSet) Len() int { return len(s) }

// ErrBackupUnsupported is returned by capability checks when a metadata store
// does not implement Backupable (ENG-04).
var ErrBackupUnsupported = errors.New("backup not supported by this metadata store")

// ErrRestoreDestinationNotEmpty is returned by Restore implementations when
// the destination store contains pre-existing data (D-06). Phase 2 drivers
// refuse to overwrite live data as a defense-in-depth measure — Phase 5's
// restore orchestrator owns all destructive prep (swap-under-temp-path,
// DROP+CREATE schema, fresh empty store construction) before calling
// Restore. A direct Restore call against a populated store is a bug and
// must fail loudly.
var ErrRestoreDestinationNotEmpty = errors.New("restore destination is not empty")

// ErrRestoreCorrupt is returned when the backup stream cannot be decoded:
// truncated archive, bit-flipped bytes, invalid frame, unknown tar entry,
// failed gob decode, etc. Drivers wrap the underlying decode error with
// fmt.Errorf("%w: %v", ErrRestoreCorrupt, cause) so callers can match via
// errors.Is while preserving the concrete cause for operator logs.
var ErrRestoreCorrupt = errors.New("restore stream is corrupt")

// ErrSchemaVersionMismatch is returned by the Postgres driver when the
// archive's schema_migrations version does not match the current binary's
// migration set. Memory and Badger drivers do not produce this error
// (they use format_version in their per-engine headers instead).
var ErrSchemaVersionMismatch = errors.New("restore archive schema version mismatch")

// ErrBackupAborted is returned when Backup is interrupted mid-stream by
// context cancellation or an unrecoverable engine error. The writer is
// left in a partial state — callers (Phase 3 destinations) must either
// discard the partial archive (tmp+rename, multipart abort) or treat it
// as corrupt. No recovery / resume semantics are offered.
var ErrBackupAborted = errors.New("backup aborted")
