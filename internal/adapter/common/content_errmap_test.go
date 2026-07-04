package common

import (
	"fmt"
	"testing"

	nfs3types "github.com/marmos91/dittofs/internal/adapter/nfs/types"
	nfs4types "github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	smbtypes "github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/local/fs"
)

// TestContentErrMap_ChunkContentMismatch verifies that a wrapped
// block.ErrChunkContentMismatch maps to I/O-class
// codes in every protocol arm. The streaming verifier rejected bytes
// before they reached the client; the protocol surfaces this as EIO
// (NFS) / unexpected I/O (SMB).
func TestContentErrMap_ChunkContentMismatch(t *testing.T) {
	wrapped := fmt.Errorf("download block cas/aa/bb/...: %w",
		block.ErrChunkContentMismatch)

	if got := MapContentToNFS3(wrapped); got != nfs3types.NFS3ErrIO {
		t.Errorf("MapContentToNFS3(ErrChunkContentMismatch) = %d, want NFS3ErrIO (%d)",
			got, nfs3types.NFS3ErrIO)
	}
	if got := MapContentToNFS4(wrapped); got != nfs4types.NFS4ERR_IO {
		t.Errorf("MapContentToNFS4(ErrChunkContentMismatch) = %d, want NFS4ERR_IO (%d)",
			got, nfs4types.NFS4ERR_IO)
	}
	if got := MapContentToSMB(wrapped); got != smbtypes.StatusUnexpectedIOError {
		t.Errorf("MapContentToSMB(ErrChunkContentMismatch) = %v, want StatusUnexpectedIOError",
			got)
	}
}

// TestContentErrMap_ChunkContentMismatch_Direct asserts the unwrapped sentinel
// also maps correctly (some call paths surface it directly).
func TestContentErrMap_ChunkContentMismatch_Direct(t *testing.T) {
	err := block.ErrChunkContentMismatch

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

// TestContentErrMap_ChunkRefMissing verifies that a wrapped
// block.ErrChunkRefMissing maps to I/O-class codes in
// every protocol arm. A ChunkRef.Hash referring to a FileChunk that
// has been GC'd or never existed is a data-integrity failure surfaced
// to the client identically to ErrChunkContentMismatch.
func TestContentErrMap_ChunkRefMissing(t *testing.T) {
	wrapped := fmt.Errorf("read block %x: %w",
		[]byte{0xab, 0xcd}, block.ErrChunkRefMissing)

	if got := MapContentToNFS3(wrapped); got != nfs3types.NFS3ErrIO {
		t.Errorf("MapContentToNFS3(ErrChunkRefMissing) = %d, want NFS3ErrIO (%d)",
			got, nfs3types.NFS3ErrIO)
	}
	if got := MapContentToNFS4(wrapped); got != nfs4types.NFS4ERR_IO {
		t.Errorf("MapContentToNFS4(ErrChunkRefMissing) = %d, want NFS4ERR_IO (%d)",
			got, nfs4types.NFS4ERR_IO)
	}
	if got := MapContentToSMB(wrapped); got != smbtypes.StatusUnexpectedIOError {
		t.Errorf("MapContentToSMB(ErrChunkRefMissing) = %v, want StatusUnexpectedIOError",
			got)
	}
}

// TestContentErrMap_ChunkRefMissing_Direct asserts the unwrapped
// sentinel maps correctly across protocols.
func TestContentErrMap_ChunkRefMissing_Direct(t *testing.T) {
	err := block.ErrChunkRefMissing

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

// TestContentErrMap_ChunkRefMissing_DistinctFromChunkContentMismatch asserts
// that ErrChunkRefMissing is its own sentinel (not merely an alias of
// ErrChunkContentMismatch). Both currently map to the same protocol
// codes (data-integrity failures) but operators distinguish them via
// log inspection — ErrChunkRefMissing means a hash is gone (GC bug
// candidate), ErrChunkContentMismatch means bytes don't match the hash
// (corruption candidate).
func TestContentErrMap_ChunkRefMissing_DistinctFromChunkContentMismatch(t *testing.T) {
	if block.ErrChunkRefMissing == block.ErrChunkContentMismatch {
		t.Fatal("ErrChunkRefMissing must be a distinct sentinel from ErrChunkContentMismatch")
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

// TestContentErrMap_PressureTimeout_SurfacedNonSuccess locks in #1267: the
// append-log backpressure sentinel fs.ErrPressureTimeout (returned when a
// wedged rollup pool cannot admit a write, and the durability-point flush the
// NFS COMMIT / SMB CLOSE handlers run cannot make forward progress) must map to
// a non-success, I/O-class status in every protocol arm. Surfacing it is what
// lets the handler fail the COMMIT/CLOSE loudly instead of acknowledging a
// payload that never durably flushed.
func TestContentErrMap_PressureTimeout_SurfacedNonSuccess(t *testing.T) {
	// Direct sentinel and a wrapped form (handlers receive the engine-wrapped
	// error) must both resolve to the same non-success codes via errors.Is.
	cases := []error{
		fs.ErrPressureTimeout,
		fmt.Errorf("engine flush payload p1: %w", fs.ErrPressureTimeout),
	}
	for _, err := range cases {
		if got := MapContentToNFS3(err); got != nfs3types.NFS3ErrIO {
			t.Errorf("MapContentToNFS3(%v) = %d, want NFS3ErrIO (%d)", err, got, nfs3types.NFS3ErrIO)
		}
		if got := MapContentToNFS3(err); got == nfs3types.NFS3OK {
			t.Errorf("MapContentToNFS3(%v) = NFS3OK — #1267: pressure-timeout flush must not look successful", err)
		}
		if got := MapContentToNFS4(err); got != nfs4types.NFS4ERR_IO {
			t.Errorf("MapContentToNFS4(%v) = %d, want NFS4ERR_IO (%d)", err, got, nfs4types.NFS4ERR_IO)
		}
		if got := MapContentToNFS4(err); got == nfs4types.NFS4_OK {
			t.Errorf("MapContentToNFS4(%v) = NFS4_OK — #1267: pressure-timeout flush must not look successful", err)
		}
		if got := MapContentToSMB(err); got != smbtypes.StatusUnexpectedIOError {
			t.Errorf("MapContentToSMB(%v) = %v, want StatusUnexpectedIOError", err, got)
		}
		if got := MapContentToSMB(err); got == smbtypes.StatusSuccess {
			t.Errorf("MapContentToSMB(%v) = StatusSuccess — #1267: pressure-timeout flush must not look successful", err)
		}
	}
}

// TestContentErrMap_NotDurableYet verifies the #1274 ErrNotDurableYet sentinel
// (data committed locally but not yet on a durable store) maps to I/O-class
// codes in every protocol arm so the client re-drives COMMIT/CLOSE, both as the
// bare sentinel and wrapped.
func TestContentErrMap_NotDurableYet(t *testing.T) {
	cases := []error{
		ErrNotDurableYet,
		fmt.Errorf("commit payload p1: %w", ErrNotDurableYet),
	}
	for _, err := range cases {
		if got := MapContentToNFS3(err); got != nfs3types.NFS3ErrIO {
			t.Errorf("MapContentToNFS3(%v) = %d, want NFS3ErrIO", err, got)
		}
		if got := MapContentToNFS4(err); got != nfs4types.NFS4ERR_IO {
			t.Errorf("MapContentToNFS4(%v) = %d, want NFS4ERR_IO", err, got)
		}
		if got := MapContentToSMB(err); got != smbtypes.StatusUnexpectedIOError {
			t.Errorf("MapContentToSMB(%v) = %v, want StatusUnexpectedIOError", err, got)
		}
		// Must never look successful.
		if MapContentToSMB(err) == smbtypes.StatusSuccess {
			t.Errorf("ErrNotDurableYet must not map to StatusSuccess")
		}
	}
}
