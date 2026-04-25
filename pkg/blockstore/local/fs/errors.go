package fs

import "errors"

// Append-log sentinel errors. Surfaced by unmarshalHeader and readLogHeader
// when the on-disk log in <baseDir>/logs/{payloadID}.log is malformed,
// corrupted, or written by an incompatible version. Consumed by recovery
// (LSL-06) in later phases of the v0.15.0 block store refactor.
var (
	// ErrLogBadMagic indicates the log header magic bytes do not match "DFLG".
	ErrLogBadMagic = errors.New("append log: bad magic")
	// ErrLogBadVersion indicates the header version is unsupported.
	ErrLogBadVersion = errors.New("append log: bad version")
	// ErrLogBadHeaderCRC indicates the header CRC does not match its payload.
	ErrLogBadHeaderCRC = errors.New("append log: bad header CRC")

	// ErrAppendLogDisabled is returned by AppendWrite when the append-log
	// path is compiled in but disabled by configuration (D-02 / D-36).
	// Through Phase 10 the flag defaults to false; Phase 11 (A2) flips it.
	ErrAppendLogDisabled = errors.New("append log: disabled (use_append_log=false)")

	// ErrDeleted is returned by AppendWrite when the payload's append log
	// has been tombstoned by a concurrent DeleteAppendLog call (D-28 /
	// plan 09). Writers that observe the tombstone short-circuit before
	// touching the log so a deleted payload never gains new records.
	ErrDeleted = errors.New("append log: payload deleted")
)
