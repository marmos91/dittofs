package metadata

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
