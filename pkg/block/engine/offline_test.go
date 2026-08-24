package engine

import "testing"

// fakeColdReporter stands in for the local tier's residency surface so the
// gating rules can be checked without building a journal.
type fakeColdReporter struct {
	seeded  bool
	bytes   int64
	extents int64
}

func (f *fakeColdReporter) ColdExtents() (int64, int64) { return f.bytes, f.extents }
func (f *fakeColdReporter) ColdSeeded() bool            { return f.seeded }

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
		wantKnown bool
		wantBytes int64
	}{
		{"no remote is local by construction", &fakeColdReporter{}, false, true, 0},
		{"tier that cannot report residency", struct{}{}, true, false, 0},
		{"unseeded tier cannot see remote-only ranges", &fakeColdReporter{seeded: false, bytes: 0}, true, false, 0},
		{"seeded and fully local", &fakeColdReporter{seeded: true}, true, true, 0},
		{"seeded with evicted ranges", &fakeColdReporter{seeded: true, bytes: 4096, extents: 2}, true, true, 4096},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := offlineReadinessOf(tc.localTier, tc.hasRemote)
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
		})
	}
}
