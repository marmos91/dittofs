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
	// has been tombstoned by a concurrent DeleteAppendLog call. Writers
	// that observe the tombstone short-circuit before touching the log so
	// a deleted payload never gains new records.
	//
	// Known limitation (#670): writers already blocked in AppendWrite's
	// pressure loop do not observe the tombstone synchronously. They
	// unblock on the next pressureCh pulse, ctx.Done(), bc.done, or
	// PressureMaxWait expiry — whichever fires first. A delete may
	// therefore surface as ErrPressureTimeout rather than ErrDeleted.
	ErrDeleted = errors.New("append log: payload deleted")

	// ErrInvalidPayloadID is returned by write-path entry points when the
	// caller supplies a payloadID that fails isValidPayloadID's structural
	// rules (empty, contains '..' / '.' / '' segments, leading '/', NUL
	// byte, or exceeds maxPayloadIDLen). Surfaced as a defense-in-depth
	// guard at getOrCreateLog: recovery already validates names read from
	// disk, but the write path previously accepted any string and passed it
	// straight to filepath.Join. A malicious or buggy caller with a '../'-
	// bearing payloadID could otherwise place a log file outside <baseDir>/
	// logs before recovery's check ever ran.
	ErrInvalidPayloadID = errors.New("append log: invalid payloadID")

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

	// ErrDrainIncomplete is returned by DrainRollups when, after a pass
	// that made no forward progress, dirty intervals remain that are backed
	// by real unflushed data — i.e. the payload is NOT tombstoned and its
	// logIndex CAN back the residual intervals, so the bytes should have
	// reached CAS but did not. Returning nil here would let snapshot-create
	// proceed to Backup with a partial manifest; surfacing this sentinel
	// lets runSnapshotOrchestration fail the snapshot visibly instead.
	//
	// Tombstoned payloads and tree/logIndex-divergent intervals (the
	// rollupFile DropExact path — bytes that never reached a chunk) are NOT
	// reported via this error: they are legitimately skipped and the drain
	// returns nil.
	ErrDrainIncomplete = errors.New("append log: drain left real unflushed dirty data")
)
