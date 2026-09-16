package handlers

import (
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/lease"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// TestSelectBreakWaitTimeout pins which bound each break path gets, and that the
// configured override reaches the traditional path only. The lease path must
// keep the compiled-in 5s bound: the MS-SMB2 §3.3.4.7 timing windows and the
// breaking3 / timeout-disconnect tests depend on it, so a configured
// oplock_break_timeout must not lengthen a lease break wait.
func TestSelectBreakWaitTimeout(t *testing.T) {
	// A nil resolver makes AnyHolderIsTraditionalOplock report false, which is
	// the lease-holder case; the traditional case is forced via the
	// share-violation reason, which takes the same bound.
	newHandler := func(configured time.Duration) *Handler {
		h := NewHandler()
		h.LeaseManager = lease.NewLeaseManager(nil, nil)
		h.OplockBreakWaitTimeout = configured
		return h
	}

	const handle = lock.FileHandle("fh-1")

	t.Run("lease path ignores the configured override", func(t *testing.T) {
		h := newHandler(90 * time.Second)
		got := h.selectBreakWaitTimeout(handle, "share", lock.BreakReasonUnspecified)
		if got != lease.AsyncCreateBreakWaitTimeout {
			t.Errorf("lease break wait = %v, want the compiled-in %v",
				got, lease.AsyncCreateBreakWaitTimeout)
		}
	})

	t.Run("traditional path uses the configured override", func(t *testing.T) {
		h := newHandler(90 * time.Second)
		got := h.selectBreakWaitTimeout(handle, "share", lock.BreakReasonSharingViolation)
		if got != 90*time.Second {
			t.Errorf("traditional break wait = %v, want 90s from the setting", got)
		}
	})

	t.Run("traditional path falls back to the default when unset", func(t *testing.T) {
		h := newHandler(0)
		got := h.selectBreakWaitTimeout(handle, "share", lock.BreakReasonSharingViolation)
		if got != lease.TraditionalOplockBreakWaitTimeout {
			t.Errorf("traditional break wait = %v, want the default %v",
				got, lease.TraditionalOplockBreakWaitTimeout)
		}
	})
}
