package shares

import (
	"fmt"
	"testing"
)

// TestWarmRegistry_RetireBoundsJobsMap verifies that terminal jobs are evicted
// once the retained window exceeds maxRetainedJobs, so a long-running server
// that warms repeatedly cannot leak the jobs map without bound.
func TestWarmRegistry_RetireBoundsJobsMap(t *testing.T) {
	r := newWarmRegistry()

	total := maxRetainedJobs + 10
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("job-%d", i)
		r.jobs[id] = &WarmJob{ID: id, State: WarmStateDone}
		r.retire(id)
	}

	if got := len(r.jobs); got > maxRetainedJobs {
		t.Errorf("jobs map size = %d, want <= %d", got, maxRetainedJobs)
	}
	// The oldest jobs are evicted; the most recent maxRetainedJobs are retained.
	if _, ok := r.jobs["job-0"]; ok {
		t.Errorf("oldest job (job-0) should have been evicted")
	}
	newest := fmt.Sprintf("job-%d", total-1)
	if _, ok := r.jobs[newest]; !ok {
		t.Errorf("newest job (%s) should still be retained for polling", newest)
	}
}
