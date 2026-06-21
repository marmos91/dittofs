package common

import (
	goerrors "errors"

	nfs3types "github.com/marmos91/dittofs/internal/adapter/nfs/types"
	nfs4types "github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	smbtypes "github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
)

// Content-error mapping.
//
// Block-store content errors (failed reads from remote/cache, transient
// backend failures) map to I/O-class codes in every protocol. The typed
// sentinels (engine.ErrStoreClosed and the block.Err* family) are matched
// here via errors.Is; every protocol arm (NFSv3/4, SMB) calls the matching
// MapContentTo* accessor so the mapping lives in exactly one place.
//
// Per-protocol fallback when the specific error is unknown:
//   - NFSv3 → NFS3ErrIO (EIO)
//   - NFSv4 → NFS4ERR_IO
//   - SMB   → StatusUnexpectedIOError (MS-ERREF 2.3)

// MapContentToNFS3 translates a block-store content error to an NFS3 code.
func MapContentToNFS3(err error) uint32 {
	if err == nil {
		return nfs3types.NFS3OK
	}
	// The block store was Closed under the in-flight op — the share was
	// removed/hot-reloaded mid-transfer. STALE tells the
	// client the handle no longer refers to a live object.
	if goerrors.Is(err, engine.ErrStoreClosed) {
		return nfs3types.NFS3ErrStale
	}
	// CAS key parse failure indicates corrupted metadata; surfaced as
	// invalid argument.
	if goerrors.Is(err, block.ErrCASKeyMalformed) {
		return nfs3types.NFS3ErrInval
	}
	// Silent S3 corruption surfaced as I/O error to the client. The
	// streaming verifier rejected bytes before they reached the caller;
	// the protocol arm reports EIO.
	if goerrors.Is(err, block.ErrCASContentMismatch) {
		return nfs3types.NFS3ErrIO
	}
	// BlockRef.Hash refers to a FileBlock that's been GC'd or never
	// existed. Operators triage via log inspection (vs.
	// ErrCASContentMismatch which means bytes don't match the hash).
	// Both surface as EIO at the wire — a data-integrity failure the
	// client must retry / refresh against.
	if goerrors.Is(err, block.ErrBlockRefMissing) {
		return nfs3types.NFS3ErrIO
	}
	if goerrors.Is(err, block.ErrRemoteUnavailable) {
		return nfs3types.NFS3ErrIO
	}
	// Data committed locally but not yet durable (volatile local store, durable
	// remote not reached) — transient I/O error so the client re-drives
	// COMMIT/CLOSE (#1274). Never occurs on the fs-local production hot path.
	if goerrors.Is(err, ErrNotDurableYet) {
		return nfs3types.NFS3ErrIO
	}
	return nfs3types.NFS3ErrIO
}

// MapContentToNFS4 translates a block-store content error to an NFS4 code.
func MapContentToNFS4(err error) uint32 {
	if err == nil {
		return nfs4types.NFS4_OK
	}
	// Closed store under an in-flight op → STALE.
	if goerrors.Is(err, engine.ErrStoreClosed) {
		return nfs4types.NFS4ERR_STALE
	}
	if goerrors.Is(err, block.ErrCASKeyMalformed) {
		return nfs4types.NFS4ERR_INVAL
	}
	if goerrors.Is(err, block.ErrCASContentMismatch) {
		return nfs4types.NFS4ERR_IO
	}
	// BlockRef hash missing (see MapContentToNFS3).
	if goerrors.Is(err, block.ErrBlockRefMissing) {
		return nfs4types.NFS4ERR_IO
	}
	if goerrors.Is(err, block.ErrRemoteUnavailable) {
		return nfs4types.NFS4ERR_IO
	}
	// Not yet durable (see MapContentToNFS3) — transient I/O so the client
	// re-drives COMMIT/CLOSE (#1274).
	if goerrors.Is(err, ErrNotDurableYet) {
		return nfs4types.NFS4ERR_IO
	}
	return nfs4types.NFS4ERR_IO
}

// MapContentToSMB translates a block-store content error to an SMB status.
func MapContentToSMB(err error) smbtypes.Status {
	if err == nil {
		return smbtypes.StatusSuccess
	}
	// Closed store under an in-flight op: the share went away
	// mid-transfer. STATUS_FILE_CLOSED is the closest SMB stale-handle
	// signal (matches the merrs.ErrStaleHandle row in errmap.go).
	if goerrors.Is(err, engine.ErrStoreClosed) {
		return smbtypes.StatusFileClosed
	}
	if goerrors.Is(err, block.ErrCASKeyMalformed) {
		return smbtypes.StatusInvalidParameter
	}
	if goerrors.Is(err, block.ErrCASContentMismatch) {
		// SMB does not have a dedicated data-checksum status that maps
		// cleanly to the client (StatusDataError is not in our types
		// table); StatusUnexpectedIOError is the closest analog and is
		// also what the existing fallback uses for opaque I/O failures.
		return smbtypes.StatusUnexpectedIOError
	}
	// BlockRef hash missing (see MapContentToNFS3).
	// SMB clients see the same StatusUnexpectedIOError signal as for
	// CAS content mismatch; both are CAS-integrity failures.
	if goerrors.Is(err, block.ErrBlockRefMissing) {
		return smbtypes.StatusUnexpectedIOError
	}
	if goerrors.Is(err, block.ErrRemoteUnavailable) {
		return smbtypes.StatusUnexpectedIOError
	}
	// Data committed locally but not yet durable (volatile local store, durable
	// remote not reached) — STATUS_UNEXPECTED_IO_ERROR so the client re-drives
	// CLOSE/flush (#1274). Never occurs on the fs-local production hot path.
	if goerrors.Is(err, ErrNotDurableYet) {
		return smbtypes.StatusUnexpectedIOError
	}
	return smbtypes.StatusUnexpectedIOError
}
