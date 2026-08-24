package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/remote"
	"github.com/marmos91/dittofs/pkg/health"
)

// TestStartScheduledIntegrityScan_TicksAndStops verifies the scheduler walks
// every registered share on its interval, records the outcome where the
// status report can find it, and exits when its context is cancelled. The
// scan itself is real — a genuine metadata walk over an (empty) share — so a
// break anywhere between the ticker and the recorded status fails here.
func TestStartScheduledIntegrityScan_TicksAndStops(t *testing.T) {
	rt := newRuntimeForGC(t, map[string]remote.RemoteStore{"/share-a": &fakeRemoteStore{name: "s3"}})

	ctx, cancel := context.WithCancel(context.Background())
	rt.StartScheduledIntegrityScan(ctx, 10*time.Millisecond)

	deadline := time.Now().Add(2 * time.Second)
	var got *health.IntegrityStatus
	for time.Now().Before(deadline) {
		if got = rt.ShareIntegrity("/share-a"); got != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got == nil {
		t.Fatal("no integrity status recorded for /share-a after 2s")
	}
	if got.Error != "" {
		t.Fatalf("scan reported an error: %s", got.Error)
	}
	if got.LastRunAt.IsZero() {
		t.Error("LastRunAt is zero; the status was recorded without a run time")
	}
	if got.DamagedPayloads != 0 {
		t.Errorf("DamagedPayloads = %d on an empty share, want 0", got.DamagedPayloads)
	}

	cancel()
	time.Sleep(30 * time.Millisecond)
	stopped := rt.ShareIntegrity("/share-a").LastRunAt
	time.Sleep(60 * time.Millisecond)
	if after := rt.ShareIntegrity("/share-a").LastRunAt; !after.Equal(stopped) {
		t.Errorf("scheduler kept running after cancel: %v -> %v", stopped, after)
	}
}

// TestWithIntegrity_DowngradeRule pins the one piece of judgement in the
// status join: damage degrades a healthy share, and never touches a share
// that is already worse off.
func TestWithIntegrity_DowngradeRule(t *testing.T) {
	healthy := health.Report{Status: health.StatusHealthy}
	unhealthy := health.Report{Status: health.StatusUnhealthy, Message: "block: remote unreachable"}

	tests := []struct {
		name       string
		rep        health.Report
		in         *health.IntegrityStatus
		wantStatus health.Status
		wantMsg    string
	}{
		{"no scan yet", healthy, nil, health.StatusHealthy, ""},
		{"clean scan", healthy, &health.IntegrityStatus{FilesScanned: 10}, health.StatusHealthy, ""},
		{"damage degrades", healthy, &health.IntegrityStatus{DamagedPayloads: 3, PayloadsWithFindings: 5}, health.StatusDegraded, "3 damaged payloads of 5"},
		{"never upgrades", unhealthy, &health.IntegrityStatus{DamagedPayloads: 3}, health.StatusUnhealthy, "block: remote unreachable"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := withIntegrity(tc.rep, tc.in)
			if got.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q", got.Status, tc.wantStatus)
			}
			if tc.wantMsg != "" && !strings.Contains(got.Message, tc.wantMsg) {
				t.Errorf("Message = %q, want it to contain %q", got.Message, tc.wantMsg)
			}
			if tc.wantMsg == "" && got.Message != "" {
				t.Errorf("Message = %q, want empty", got.Message)
			}
			if got.Integrity != tc.in {
				t.Error("Integrity was not carried through to the status")
			}
		})
	}
}

// TestIntegritySnapshot_FailedScanIsNotNeverScanned pins the distinction the
// metrics surface has to keep: a scan that fails publishes no counts and no
// completion time, exactly like a share never scanned, so the failure has to
// be flagged separately or a permanently-erroring scanner reads as a disabled
// one forever.
func TestIntegritySnapshot_FailedScanIsNotNeverScanned(t *testing.T) {
	never := integritySnapshot(nil)
	if never.LastScanUnix != 0 || never.LastScanFailed {
		t.Errorf("never scanned = %+v, want zero timestamp and failed=false", never)
	}

	failed := integritySnapshot(&health.IntegrityStatus{
		LastRunAt: time.Now().UTC(),
		Error:     "metadata store unavailable",
	})
	if !failed.LastScanFailed {
		t.Error("a failed scan did not set LastScanFailed")
	}
	if failed.LastScanUnix != 0 {
		t.Errorf("LastScanUnix = %d after a failed scan, want 0 (nothing completed)", failed.LastScanUnix)
	}
	if failed.FilesScanned != 0 || failed.DamagedPayloads != 0 {
		t.Errorf("failed scan published counts: %+v", failed)
	}

	ok := integritySnapshot(&health.IntegrityStatus{
		LastRunAt:       time.Unix(1700000000, 0).UTC(),
		FilesScanned:    42,
		DamagedPayloads: 1,
	})
	if ok.LastScanFailed {
		t.Error("a completed scan was flagged as failed")
	}
	if ok.LastScanUnix != 1700000000 || ok.FilesScanned != 42 || ok.DamagedPayloads != 1 {
		t.Errorf("completed scan = %+v, want the recorded values", ok)
	}
}
