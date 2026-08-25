package engine

import (
	"context"
	"errors"
	"testing"
)

// fakeColdReporter stands in for the local tier's residency surface so the
// gating rules can be checked without building a journal.
type fakeColdReporter struct {
	seeded  bool
	bytes   int64
	extents int64
	err     error
}

func (f *fakeColdReporter) ColdExtents(ctx context.Context) (int64, int64, error) {
	if f.err != nil {
		return 0, 0, f.err
	}
	return f.bytes, f.extents, ctx.Err()
}
func (f *fakeColdReporter) ColdSeeded() bool { return f.seeded }

func (f *fakeColdReporter) DataExtents(context.Context, string, int64) ([][2]uint64, error) {
	return nil, nil
}

// noShortfall stands in for a manifest that accounts for every range the index
// describes, so the gating rules below are exercised on their own.
func noShortfall(context.Context, coldRangeReporter) (int64, int64, error) { return 0, 0, nil }

// TestOfflineReadiness_Safe pins the two ways a readiness answer can be wrong
// in the dangerous direction: reporting zero remote-only bytes for a share
// whose residency is not actually known reads as "provably offline safe" for
// exactly the shares most likely not to be.
func TestOfflineReadiness_Safe(t *testing.T) {
	tests := []struct {
		name     string
		r        OfflineReadiness
		wantSafe bool
	}{
		{"known and fully local", OfflineReadiness{Known: true}, true},
		{"known with remote-only bytes", OfflineReadiness{Known: true, RemoteOnlyBytes: 1}, false},
		{"unknown reads as unsafe", OfflineReadiness{Reason: "local tier has not been seeded from the manifest"}, false},
		{"unknown is never safe even at zero", OfflineReadiness{Reason: "block store is closed", RemoteOnlyBytes: 0}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.r.Safe(); got != tc.wantSafe {
				t.Errorf("Safe() = %v, want %v", got, tc.wantSafe)
			}
		})
	}
}

// TestOfflineReadinessOf_Gating pins when the measurement refuses to answer.
// Each refusal replaces a zero that would have read as "provably offline
// safe", which is the wrong answer in the dangerous direction.
func TestOfflineReadinessOf_Gating(t *testing.T) {
	tests := []struct {
		name      string
		localTier any
		hasRemote bool
		shortfall shortfallFunc
		wantKnown bool
		wantBytes int64
	}{
		{"no remote, tier confirms nothing evicted", &fakeColdReporter{}, false, noShortfall, true, 0},
		{"tier that cannot report residency", struct{}{}, true, noShortfall, false, 0},
		{"no remote and no residency tracking", struct{}{}, false, noShortfall, true, 0},
		{"unseeded tier cannot see remote-only ranges", &fakeColdReporter{seeded: false, bytes: 0}, true, noShortfall, false, 0},
		{"seeded and fully local", &fakeColdReporter{seeded: true}, true, noShortfall, true, 0},
		{"seeded with evicted ranges", &fakeColdReporter{seeded: true, bytes: 4096, extents: 2}, true, noShortfall, true, 4096},
		// Unbinding a remote from a share that had already evicted leaves the
		// cold intervals behind, and the journal replays them from its cold log
		// on the next open. Reads reconcile on the cold flag whether or not a
		// remote exists, so those ranges never serve — reporting this share
		// safe would be the worst answer the measurement can give.
		{"no remote but cold ranges remain", &fakeColdReporter{seeded: false, bytes: 8192, extents: 3}, false, noShortfall, true, 8192},
		// A range the manifest places that no interval describes was lost, not
		// evicted: it adds nothing to the cold tally, so without this the
		// answer below would be a confident zero.
		{"manifest places bytes the index has forgotten", &fakeColdReporter{seeded: true}, true,
			func(context.Context, coldRangeReporter) (int64, int64, error) { return 4096, 1, nil }, false, 0},
		{"cross-check could not run", &fakeColdReporter{seeded: true}, true,
			func(context.Context, coldRangeReporter) (int64, int64, error) { return 0, 0, errNoManifest }, false, 0},
		{"no manifest to cross-check against", &fakeColdReporter{seeded: true}, true, nil, false, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := offlineReadinessOf(context.Background(), tc.localTier, tc.hasRemote, tc.shortfall)
			if got.Known != tc.wantKnown {
				t.Errorf("Known = %v, want %v (reason %q)", got.Known, tc.wantKnown, got.Reason)
			}
			if got.RemoteOnlyBytes != tc.wantBytes {
				t.Errorf("RemoteOnlyBytes = %d, want %d", got.RemoteOnlyBytes, tc.wantBytes)
			}
			if !got.Known && got.Reason == "" {
				t.Error("refused to answer without saying why")
			}
			if got.Known && got.Reason != "" {
				t.Errorf("answered but still gave a reason: %q", got.Reason)
			}
			if got.Safe() && got.RemoteOnlyBytes != 0 {
				t.Errorf("reported safe with %d remote-only bytes", got.RemoteOnlyBytes)
			}
		})
	}
}

// TestOfflineReadinessOf_CancelledScanIsUnknown asserts a walk that ran out of
// time reports unknown rather than the partial total it had reached. A status
// request and a metrics scrape both carry a deadline, and a truncated count
// would read as a share holding less remote-only data than it does.
func TestOfflineReadinessOf_CancelledScanIsUnknown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got := offlineReadinessOf(ctx, &fakeColdReporter{seeded: true, bytes: 4096, extents: 2}, true, noShortfall)
	if got.Known {
		t.Error("a cancelled scan reported a known answer")
	}
	if got.Safe() {
		t.Error("a cancelled scan reported the share offline-safe")
	}
	if got.RemoteOnlyBytes != 0 {
		t.Errorf("RemoteOnlyBytes = %d after cancellation, want 0 (no partial totals)", got.RemoteOnlyBytes)
	}
	if got.Reason == "" {
		t.Error("refused to answer without saying why")
	}

	// A scan that fails for its own reasons is the same kind of non-answer.
	failed := offlineReadinessOf(context.Background(),
		&fakeColdReporter{seeded: true, err: errors.New("store is closed")}, true, noShortfall)
	if failed.Known || failed.Safe() {
		t.Errorf("a failed scan reported known=%v safe=%v", failed.Known, failed.Safe())
	}
}
