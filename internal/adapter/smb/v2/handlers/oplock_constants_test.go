package handlers

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// TestOplockLevelRank_Ordering pins the strength ordering used by the
// durable-reconnect fallback: when LeaseManager re-registration under-delivers
// (returns a weaker level than what was persisted with the durable handle), the
// reconnect response must fall back to the persisted level rather than report a
// degraded oplock (smb2.durable-v2-open.lock-oplock io.out.oplock_level).
func TestOplockLevelRank_Ordering(t *testing.T) {
	t.Parallel()

	if oplockLevelRank(OplockLevelBatch) <= oplockLevelRank(OplockLevelExclusive) {
		t.Error("Batch must outrank Exclusive")
	}
	if oplockLevelRank(OplockLevelExclusive) <= oplockLevelRank(OplockLevelII) {
		t.Error("Exclusive must outrank II")
	}
	if oplockLevelRank(OplockLevelII) <= oplockLevelRank(OplockLevelNone) {
		t.Error("II must outrank None")
	}
}

// TestOplockLevelRank_FallbackDecision exercises the exact decision the
// reconnect path makes: report the re-granted level only when it is at least as
// strong as the persisted level; otherwise fall back to the persisted level.
func TestOplockLevelRank_FallbackDecision(t *testing.T) {
	t.Parallel()

	report := func(regranted, persisted uint8) uint8 {
		if oplockLevelRank(regranted) >= oplockLevelRank(persisted) {
			return regranted
		}
		return persisted
	}

	tests := []struct {
		name      string
		regranted uint8
		persisted uint8
		want      uint8
	}{
		{"regrant zero falls back to persisted Batch", OplockLevelNone, OplockLevelBatch, OplockLevelBatch},
		{"regrant weaker falls back to persisted Batch", OplockLevelII, OplockLevelBatch, OplockLevelBatch},
		{"regrant equal keeps Batch", OplockLevelBatch, OplockLevelBatch, OplockLevelBatch},
		{"regrant stronger is reported", OplockLevelBatch, OplockLevelII, OplockLevelBatch},
		{"exclusive persisted, zero regrant", OplockLevelNone, OplockLevelExclusive, OplockLevelExclusive},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := report(tt.regranted, tt.persisted); got != tt.want {
				t.Errorf("report(%#x, %#x) = %#x, want %#x", tt.regranted, tt.persisted, got, tt.want)
			}
		})
	}
}

// TestLeaseStateRank_Ordering pins the lease-state strength ordering (R < RW <
// RWH) used by the durable-reconnect lease fallback so a re-granted state that
// is weaker than the persisted state is replaced by the persisted state in the
// reconnect lease response (smb2.durable-v2-open.lock-lease).
func TestLeaseStateRank_Ordering(t *testing.T) {
	t.Parallel()

	r := uint32(lock.LeaseStateRead)
	rw := uint32(lock.LeaseStateRead | lock.LeaseStateWrite)
	rwh := uint32(lock.LeaseStateRead | lock.LeaseStateWrite | lock.LeaseStateHandle)
	none := uint32(lock.LeaseStateNone)

	if leaseStateRank(rwh) <= leaseStateRank(rw) {
		t.Error("RWH must outrank RW")
	}
	if leaseStateRank(rw) <= leaseStateRank(r) {
		t.Error("RW must outrank R")
	}
	if leaseStateRank(r) <= leaseStateRank(none) {
		t.Error("R must outrank None")
	}
}

// TestLeaseStateRank_FallbackDecision exercises the reconnect lease decision:
// a persisted RWH lease must be reported even when re-registration grants a
// weaker (or zero) state.
func TestLeaseStateRank_FallbackDecision(t *testing.T) {
	t.Parallel()

	report := func(granted, persisted uint32) uint32 {
		reported := granted
		if persisted != lock.LeaseStateNone &&
			leaseStateRank(persisted) > leaseStateRank(reported) {
			reported = persisted
		}
		return reported
	}

	rwh := uint32(lock.LeaseStateRead | lock.LeaseStateWrite | lock.LeaseStateHandle)
	rh := uint32(lock.LeaseStateRead | lock.LeaseStateHandle)
	none := uint32(lock.LeaseStateNone)

	if got := report(none, rwh); got != rwh {
		t.Errorf("zero grant with persisted RWH: got %#x, want RWH %#x", got, rwh)
	}
	if got := report(rh, rwh); got != rwh {
		t.Errorf("RH grant with persisted RWH: got %#x, want RWH %#x", got, rwh)
	}
	if got := report(rwh, rh); got != rwh {
		t.Errorf("RWH grant with persisted RH: got %#x, want RWH %#x", got, rwh)
	}
}
