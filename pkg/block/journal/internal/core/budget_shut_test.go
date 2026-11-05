package core_test

import (
	"fmt"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// name gives a segment's file the name the cache's own checks use, so the
// two caches this file builds share one shape of segment name with them.
func name(id core.SegmentID) string { return fmt.Sprintf("%020d.seg", id) }

// TestSegmentShutdownUnregistersFromTheBudget checks the registry gauge:
// two caches on one budget register two reclaimers, and each Shutdown removes
// its own, so a shut journal leaves no closure in the budget's round-robin —
// one that would pin the whole cache and be asked on every Reclaim.
//
// It lives here rather than beside the segment cache because the gauge rides
// the test probe in package core, which no other package's test binary sees:
// only core's own tests and this, core's external test package, link it.
func TestSegmentShutdownUnregistersFromTheBudget(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seam, err := journaltest.NewSeam(t.TempDir())
	if err != nil {
		t.Fatalf("the seam could not be made: %v", err)
	}
	budget := core.NewDescriptorBudget(4)
	c1 := segment.NewDescriptors(seam.FS(), name, budget)
	c2 := segment.NewDescriptors(seam.FS(), name, budget)

	if got := core.Reclaimers(budget); got != 2 {
		t.Fatalf("the budget holds %d reclaimers with two caches, want 2", got)
	}
	if err := c1.Shutdown(); err != nil {
		t.Fatalf("Shutdown of the first cache = %v, want nil", err)
	}
	if got := core.Reclaimers(budget); got != 1 {
		t.Fatalf("the budget holds %d reclaimers after one cache shut down, want 1: Shutdown must unregister its closure", got)
	}
	if err := c2.Shutdown(); err != nil {
		t.Fatalf("Shutdown of the second cache = %v, want nil", err)
	}
	if got := core.Reclaimers(budget); got != 0 {
		t.Fatalf("the budget holds %d reclaimers after both caches shut down, want 0", got)
	}
	if budget.Reclaim() {
		t.Fatal("Reclaim with no reclaimer returned true, want false")
	}
}
