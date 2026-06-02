package common

import (
	"fmt"
	"testing"

	nfs3types "github.com/marmos91/dittofs/internal/adapter/nfs/types"
	nfs4types "github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	smbtypes "github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/block"
)

// TestContentErrMap_CASMismatch verifies that a wrapped
// block.ErrCASContentMismatch maps to I/O-class
// codes in every protocol arm. The streaming verifier rejected bytes
// before they reached the client; the protocol surfaces this as EIO
// (NFS) / unexpected I/O (SMB).
func TestContentErrMap_CASMismatch(t *testing.T) {
	wrapped := fmt.Errorf("download block cas/aa/bb/...: %w",
		block.ErrCASContentMismatch)

	if got := MapContentToNFS3(wrapped); got != nfs3types.NFS3ErrIO {
		t.Errorf("MapContentToNFS3(ErrCASContentMismatch) = %d, want NFS3ErrIO (%d)",
			got, nfs3types.NFS3ErrIO)
	}
	if got := MapContentToNFS4(wrapped); got != nfs4types.NFS4ERR_IO {
		t.Errorf("MapContentToNFS4(ErrCASContentMismatch) = %d, want NFS4ERR_IO (%d)",
			got, nfs4types.NFS4ERR_IO)
	}
	if got := MapContentToSMB(wrapped); got != smbtypes.StatusUnexpectedIOError {
		t.Errorf("MapContentToSMB(ErrCASContentMismatch) = %v, want StatusUnexpectedIOError",
			got)
	}
}

// TestContentErrMap_CASMismatch_Direct asserts the unwrapped sentinel
// also maps correctly (some call paths surface it directly).
func TestContentErrMap_CASMismatch_Direct(t *testing.T) {
	err := block.ErrCASContentMismatch

	if got := MapContentToNFS3(err); got != nfs3types.NFS3ErrIO {
		t.Errorf("MapContentToNFS3 direct = %d, want NFS3ErrIO", got)
	}
	if got := MapContentToNFS4(err); got != nfs4types.NFS4ERR_IO {
		t.Errorf("MapContentToNFS4 direct = %d, want NFS4ERR_IO", got)
	}
	if got := MapContentToSMB(err); got != smbtypes.StatusUnexpectedIOError {
		t.Errorf("MapContentToSMB direct = %v, want StatusUnexpectedIOError", got)
	}
}

// TestContentErrMap_CASKeyMalformed verifies that a wrapped
// block.ErrCASKeyMalformed maps to invalid-argument
// codes — the metadata describing the CAS object is corrupt.
func TestContentErrMap_CASKeyMalformed(t *testing.T) {
	wrapped := fmt.Errorf("parse cas key %q: %w",
		"cas/zz/yy/notvalid", block.ErrCASKeyMalformed)

	if got := MapContentToNFS3(wrapped); got != nfs3types.NFS3ErrInval {
		t.Errorf("MapContentToNFS3(ErrCASKeyMalformed) = %d, want NFS3ErrInval (%d)",
			got, nfs3types.NFS3ErrInval)
	}
	if got := MapContentToNFS4(wrapped); got != nfs4types.NFS4ERR_INVAL {
		t.Errorf("MapContentToNFS4(ErrCASKeyMalformed) = %d, want NFS4ERR_INVAL (%d)",
			got, nfs4types.NFS4ERR_INVAL)
	}
	if got := MapContentToSMB(wrapped); got != smbtypes.StatusInvalidParameter {
		t.Errorf("MapContentToSMB(ErrCASKeyMalformed) = %v, want StatusInvalidParameter",
			got)
	}
}

// TestContentErrMap_CASKeyMalformed_Direct asserts the unwrapped
// sentinel maps correctly.
func TestContentErrMap_CASKeyMalformed_Direct(t *testing.T) {
	err := block.ErrCASKeyMalformed

	if got := MapContentToNFS3(err); got != nfs3types.NFS3ErrInval {
		t.Errorf("MapContentToNFS3 direct = %d, want NFS3ErrInval", got)
	}
	if got := MapContentToNFS4(err); got != nfs4types.NFS4ERR_INVAL {
		t.Errorf("MapContentToNFS4 direct = %d, want NFS4ERR_INVAL", got)
	}
	if got := MapContentToSMB(err); got != smbtypes.StatusInvalidParameter {
		t.Errorf("MapContentToSMB direct = %v, want StatusInvalidParameter", got)
	}
}

// TestContentErrMap_BlockRefMissing verifies that a wrapped
// block.ErrBlockRefMissing maps to I/O-class codes in
// every protocol arm. A BlockRef.Hash referring to a FileBlock that
// has been GC'd or never existed is a data-integrity failure surfaced
// to the client identically to ErrCASContentMismatch.
func TestContentErrMap_BlockRefMissing(t *testing.T) {
	wrapped := fmt.Errorf("read block %x: %w",
		[]byte{0xab, 0xcd}, block.ErrBlockRefMissing)

	if got := MapContentToNFS3(wrapped); got != nfs3types.NFS3ErrIO {
		t.Errorf("MapContentToNFS3(ErrBlockRefMissing) = %d, want NFS3ErrIO (%d)",
			got, nfs3types.NFS3ErrIO)
	}
	if got := MapContentToNFS4(wrapped); got != nfs4types.NFS4ERR_IO {
		t.Errorf("MapContentToNFS4(ErrBlockRefMissing) = %d, want NFS4ERR_IO (%d)",
			got, nfs4types.NFS4ERR_IO)
	}
	if got := MapContentToSMB(wrapped); got != smbtypes.StatusUnexpectedIOError {
		t.Errorf("MapContentToSMB(ErrBlockRefMissing) = %v, want StatusUnexpectedIOError",
			got)
	}
}

// TestContentErrMap_BlockRefMissing_Direct asserts the unwrapped
// sentinel maps correctly across protocols.
func TestContentErrMap_BlockRefMissing_Direct(t *testing.T) {
	err := block.ErrBlockRefMissing

	if got := MapContentToNFS3(err); got != nfs3types.NFS3ErrIO {
		t.Errorf("MapContentToNFS3 direct = %d, want NFS3ErrIO", got)
	}
	if got := MapContentToNFS4(err); got != nfs4types.NFS4ERR_IO {
		t.Errorf("MapContentToNFS4 direct = %d, want NFS4ERR_IO", got)
	}
	if got := MapContentToSMB(err); got != smbtypes.StatusUnexpectedIOError {
		t.Errorf("MapContentToSMB direct = %v, want StatusUnexpectedIOError", got)
	}
}

// TestContentErrMap_BlockRefMissing_DistinctFromCASMismatch asserts
// that ErrBlockRefMissing is its own sentinel (not merely an alias of
// ErrCASContentMismatch). Both currently map to the same protocol
// codes (data-integrity failures) but operators distinguish them via
// log inspection — ErrBlockRefMissing means a hash is gone (GC bug
// candidate), ErrCASContentMismatch means bytes don't match the hash
// (corruption candidate).
func TestContentErrMap_BlockRefMissing_DistinctFromCASMismatch(t *testing.T) {
	if block.ErrBlockRefMissing == block.ErrCASContentMismatch {
		t.Fatal("ErrBlockRefMissing must be a distinct sentinel from ErrCASContentMismatch")
	}
}

// TestContentErrMap_NonCAS_NoRegression asserts that errors which do
// NOT wrap either CAS sentinel still flow through the pre-existing
// table — ErrRemoteUnavailable and unknown errors map to I/O codes.
func TestContentErrMap_NonCAS_NoRegression(t *testing.T) {
	// ErrRemoteUnavailable: existing entry, must continue to behave.
	if got := MapContentToNFS3(block.ErrRemoteUnavailable); got != nfs3types.NFS3ErrIO {
		t.Errorf("regression: MapContentToNFS3(ErrRemoteUnavailable) = %d, want NFS3ErrIO", got)
	}
	if got := MapContentToNFS4(block.ErrRemoteUnavailable); got != nfs4types.NFS4ERR_IO {
		t.Errorf("regression: MapContentToNFS4(ErrRemoteUnavailable) = %d, want NFS4ERR_IO", got)
	}
	if got := MapContentToSMB(block.ErrRemoteUnavailable); got != smbtypes.StatusUnexpectedIOError {
		t.Errorf("regression: MapContentToSMB(ErrRemoteUnavailable) = %v, want StatusUnexpectedIOError", got)
	}

	// Unknown error: fallback I/O. Already covered by existing tests
	// (TestMapContentToNFS3 etc.) but we re-assert here to make the
	// regression intent explicit alongside the new CAS rows.
	other := fmt.Errorf("opaque storage failure")
	if got := MapContentToNFS3(other); got != nfs3types.NFS3ErrIO {
		t.Errorf("regression: MapContentToNFS3(unknown) = %d, want NFS3ErrIO", got)
	}
}
