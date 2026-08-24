package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	"github.com/marmos91/dittofs/pkg/health"
)

// newRuntimeForSkipStatus builds the smallest Runtime that can answer a share
// status probe: HealthcheckShare only reaches the shares registry before it
// decides, so no stores, metadata service or block stores are needed.
func newRuntimeForSkipStatus() *Runtime {
	return &Runtime{sharesSvc: shares.New()}
}

// A share the runtime refused to serve is absent from the shares registry
// exactly like a share that was never configured. Without a recorded reason
// both report StatusUnknown "share not found", which is what made a boot-time
// refusal invisible to an operator reading the API or the CLI.
func TestHealthcheckShare_SkippedReportsUnhealthyWithReason(t *testing.T) {
	rt := newRuntimeForSkipStatus()

	// Baseline: an unknown share is indeterminate, not unhealthy. This is the
	// contract the skip reason has to stay distinguishable from.
	rep := rt.HealthcheckShare(context.Background(), "/never-configured")
	if rep.Status != health.StatusUnknown {
		t.Errorf("unconfigured share status = %s, want unknown", rep.Status)
	}

	const reason = "master key is Deactivated at the HSM"
	rt.markShareSkipped("/refused", reason)

	rep = rt.HealthcheckShare(context.Background(), "/refused")
	if rep.Status != health.StatusUnhealthy {
		t.Errorf("skipped share status = %s, want unhealthy", rep.Status)
	}
	if !strings.Contains(rep.Message, reason) {
		t.Errorf("message = %q, want it to carry %q", rep.Message, reason)
	}
}

// A recorded refusal must not outlive its subject: once the same name is
// serving, or is gone, the stale boot-time reason has to stop being reported.
func TestHealthcheckShare_SkipReasonClearedOnAddAndRemove(t *testing.T) {
	rt := newRuntimeForSkipStatus()

	rt.markShareSkipped("/s", "remote endpoint unreachable")
	if _, skipped := rt.shareSkipReason("/s"); !skipped {
		t.Fatal("expected the skip reason to be recorded")
	}

	rt.clearShareState("/s")
	if _, skipped := rt.shareSkipReason("/s"); skipped {
		t.Error("expected the skip reason to be cleared")
	}

	rep := rt.HealthcheckShare(context.Background(), "/s")
	if rep.Status != health.StatusUnknown {
		t.Errorf("cleared share status = %s, want unknown", rep.Status)
	}
}

// Clearing a name that was never skipped must not disturb other entries, and
// reading an absent name must not report a reason.
func TestShareSkipReason_IsPerShare(t *testing.T) {
	rt := newRuntimeForSkipStatus()

	rt.markShareSkipped("/a", "reason a")
	rt.clearShareState("/b")

	if reason, skipped := rt.shareSkipReason("/a"); !skipped || reason != "reason a" {
		t.Errorf("shareSkipReason(/a) = (%q, %v), want (\"reason a\", true)", reason, skipped)
	}
	if _, skipped := rt.shareSkipReason("/b"); skipped {
		t.Error("shareSkipReason(/b) reported a reason that was never recorded")
	}
}
