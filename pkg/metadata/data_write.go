package metadata

import (
	"context"
	"time"
)

// DataWriteApplier is the hot-path metadata update for a data WRITE. A store
// transaction implements it to collapse the read-modify-write a WRITE performs
// — grow size to max(old, new), stamp mtime/ctime, optionally clear setuid/setgid
// — into a single narrow statement, instead of GetFile (full-row read) plus
// PutFile (full-row rewrite). It returns the resulting size so the caller can
// synthesize post-op attrs without reading the row back.
//
// Optional: transactions that do not implement it fall back to GetFile+PutFile.
// Only regular files are affected: a missing target returns ErrNotFound, and a
// target that exists but is not a regular file returns ErrIsDirectory and is
// left untouched.
type DataWriteApplier interface {
	ApplyDataWrite(ctx context.Context, handle FileHandle, newSize uint64, now time.Time, clearSUID bool) (finalSize uint64, err error)
}
