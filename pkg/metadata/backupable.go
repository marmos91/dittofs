package metadata

import (
	"context"
	"errors"
	"io"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// Backupable is an optional capability that metadata store backends may
// implement to support share-level backup and restore. It is deliberately
// NOT embedded in MetadataStore so that protocol handlers and the runtime
// never depend on backup support existing.
//
// Call sites discover the capability via a type assertion:
//
//	if b, ok := store.(metadata.Backupable); ok {
//	    hashes, err := b.Backup(ctx, w)
//	    ...
//	}
//
// Backup writes engine-specific metadata into w and returns the set of
// block hashes referenced by the snapshot. The caller is responsible for
// placing GC holds on those hashes before the backup stream is considered
// durable.
//
// Restore reads a previously-backed-up stream from r and rebuilds the
// metadata state. The destination store must be empty (no existing share
// data); otherwise ErrRestoreDestinationNotEmpty is returned.
type Backupable interface {
	// Backup serializes all metadata into w and returns the set of
	// content-addressed block hashes referenced by the snapshot.
	Backup(ctx context.Context, w io.Writer) (*blockstore.HashSet, error)

	// Restore reads a backup stream from r and rebuilds metadata state.
	// The store must be empty; returns ErrRestoreDestinationNotEmpty otherwise.
	Restore(ctx context.Context, r io.Reader) error
}

// Backup/restore error sentinels. Callers detect these via errors.Is
// through any wrapping depth.
var (
	// ErrRestoreDestinationNotEmpty is returned by Restore when the target
	// store already contains data. The caller must provide an empty store.
	ErrRestoreDestinationNotEmpty = errors.New("metadata: restore destination is not empty")

	// ErrRestoreCorrupt is returned by Restore when the input stream
	// fails integrity checks (bad CRC, truncated envelope, malformed
	// engine payload).
	ErrRestoreCorrupt = errors.New("metadata: restore data is corrupt")

	// ErrSchemaVersionMismatch is returned by Restore when the backup
	// stream's schema version does not match the running engine's expected
	// version. The operator must upgrade or downgrade the server before
	// restoring.
	ErrSchemaVersionMismatch = errors.New("metadata: schema version mismatch")

	// ErrBackupAborted is returned by Backup when the operation is
	// cancelled (context done) or otherwise cannot complete. Partial
	// output written to w must be discarded by the caller.
	ErrBackupAborted = errors.New("metadata: backup aborted")
)
