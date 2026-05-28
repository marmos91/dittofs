package fs

import "errors"

// Append-log sentinel errors. Surfaced by unmarshalHeader and readLogHeader
// when the on-disk log in <baseDir>/logs/{payloadID}.log is malformed
// corrupted, or written by an incompatible version. Consumed by recovery
// in later phases of the v0.15.0 block store refactor.
var (
	// ErrLogBadMagic indicates the log header magic bytes do not match "DFLG".
	ErrLogBadMagic = errors.New("append log: bad magic")
	// ErrLogBadVersion indicates the header version is unsupported.
	ErrLogBadVersion = errors.New("append log: bad version")
	// ErrLogBadHeaderCRC indicates the header CRC does not match its payload.
	ErrLogBadHeaderCRC = errors.New("append log: bad header CRC")

	// ErrDeleted is returned by AppendWrite when the payload's append log
	// has been tombstoned by a concurrent DeleteAppendLog call (/
	// ). Writers that observe the tombstone short-circuit before
	// touching the log so a deleted payload never gains new records.
	ErrDeleted = errors.New("append log: payload deleted")

	// ErrPressureTimeout is returned by AppendWrite when its pressure
	// loop has waited longer than FSStoreOptions.PressureMaxWait without
	// rollup freeing enough log budget to admit the write. Distinguished
	// from ctx.Err() (caller-imposed deadline) and ErrStoreClosed (the
	// store is shutting down): ErrPressureTimeout specifically means the
	// rollup pool is making no forward progress (#670).
	//
	// NFS COMMIT and SMB Flush typically arrive with no caller-supplied
	// deadline (or one measured in minutes), so a wedged rollup translates
	// into D-state on the client. Surfacing this sentinel lets the protocol
	// adapter decide whether to map it to NFS3ERR_SERVERFAULT /
	// STATUS_INTERNAL_ERROR, log, and release the goroutine instead of
	// blocking indefinitely.
	ErrPressureTimeout = errors.New("append log: pressure wait timed out")
)
