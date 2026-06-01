package nsm

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/nsm/handlers"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
	"github.com/stretchr/testify/require"
)

// TestNotifyAllClients_OutboundFailure_DoesNotReleaseLocks proves that a failed
// OUTBOUND SM_NOTIFY on server restart does NOT trigger crash cleanup. A notify
// failure means the notification did not reach the client (down, network blip,
// changed ephemeral port) — it is not crash evidence. Releasing the client's
// locks here would defeat the reclaim grace window for the very clients it
// protects (NFS H-D / NSM grace regression). Locks must be left intact for the
// grace timer + onGraceEnd sweep to age out.
func TestNotifyAllClients_OutboundFailure_DoesNotReleaseLocks(t *testing.T) {
	t.Parallel()

	tracker := lock.NewConnectionTracker(lock.ConnectionTrackerConfig{})
	require.NoError(t, tracker.RegisterClient("clientA", "nlm", "10.0.0.1:0", 0))
	// Mark the client as NSM-monitored with a callback target that cannot be
	// reached, so SendNotify fails.
	tracker.UpdateNSMInfo("clientA", "clientA", [16]byte{}, &lock.NSMCallback{
		Hostname: "192.0.2.1:0", // TEST-NET-1, guaranteed unroutable
		Program:  100021,
		Version:  4,
		Proc:     1,
	})

	h := handlers.NewHandler(handlers.HandlerConfig{
		Tracker:    tracker,
		ServerName: "testserver",
	})

	var crashCalls int32
	n := NewNotifier(NotifierConfig{
		Handler:    h,
		ServerName: "testserver",
		OnClientCrash: func(_ context.Context, _ string) error {
			atomic.AddInt32(&crashCalls, 1)
			return nil
		},
	})

	results := n.NotifyAllClients(context.Background())

	require.Len(t, results, 1, "exactly one client should be notified")
	require.Error(t, results[0].Error, "outbound notify to unroutable host must fail")
	require.Equal(t, int32(0), atomic.LoadInt32(&crashCalls),
		"a failed outbound SM_NOTIFY must NOT trigger crash cleanup")
}

// TestDetectCrash_StillReleasesLocks proves the INBOUND crash-evidence path
// (DetectCrash) still releases locks — the fix only removes the outbound-notify
// path, not legitimate crash handling.
func TestDetectCrash_StillReleasesLocks(t *testing.T) {
	t.Parallel()

	tracker := lock.NewConnectionTracker(lock.ConnectionTrackerConfig{})
	require.NoError(t, tracker.RegisterClient("clientA", "nlm", "10.0.0.1:0", 0))

	h := handlers.NewHandler(handlers.HandlerConfig{
		Tracker:    tracker,
		ServerName: "testserver",
	})

	var crashCalls int32
	n := NewNotifier(NotifierConfig{
		Handler:    h,
		ServerName: "testserver",
		OnClientCrash: func(_ context.Context, _ string) error {
			atomic.AddInt32(&crashCalls, 1)
			return nil
		},
	})

	n.DetectCrash(context.Background(), "clientA")

	require.Equal(t, int32(1), atomic.LoadInt32(&crashCalls),
		"inbound crash evidence must still trigger crash cleanup")
}
