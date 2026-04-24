package common

import (
	goerrors "errors"

	nfs3types "github.com/marmos91/dittofs/internal/adapter/nfs/types"
	nfs4types "github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	smbtypes "github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/blockstore"
)

// Content-error mapping (D-08 §2).
//
// Block-store content errors (failed reads from remote/cache, transient
// backend failures) map to I/O-class codes in every protocol. Today the
// only typed signal is blockstore.ErrRemoteUnavailable; a string-match
// heuristic for "cache full" is intentionally kept OUT of common/ and
// stays at the NFSv3 call site (see internal/adapter/nfs/xdr/errors.go:
// MapContentErrorToNFSStatus) — the typed-error path lives here.
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
	if goerrors.Is(err, blockstore.ErrRemoteUnavailable) {
		return nfs3types.NFS3ErrIO
	}
	return nfs3types.NFS3ErrIO
}

// MapContentToNFS4 translates a block-store content error to an NFS4 code.
func MapContentToNFS4(err error) uint32 {
	if err == nil {
		return nfs4types.NFS4_OK
	}
	if goerrors.Is(err, blockstore.ErrRemoteUnavailable) {
		return nfs4types.NFS4ERR_IO
	}
	return nfs4types.NFS4ERR_IO
}

// MapContentToSMB translates a block-store content error to an SMB status.
func MapContentToSMB(err error) smbtypes.Status {
	if err == nil {
		return smbtypes.StatusSuccess
	}
	if goerrors.Is(err, blockstore.ErrRemoteUnavailable) {
		return smbtypes.StatusUnexpectedIOError
	}
	return smbtypes.StatusUnexpectedIOError
}
