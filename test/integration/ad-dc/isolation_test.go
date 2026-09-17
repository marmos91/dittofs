//go:build ad_dc

package ad_dc_test

import (
	"strings"
	"testing"
)

// TestAdContainerNamesAreDistinct guards the fixture-isolation invariant: each
// test must address its own container, so no test's teardown can remove the DC
// out from under another test's in-flight `docker exec`. A regression here would
// make uniqueContainerName return a fixed or shared name again.
func TestAdContainerNamesAreDistinct(t *testing.T) {
	t.Run("alpha", func(t *testing.T) {
		if got := uniqueContainerName(t); got != "dittofs-ad-dc-test-testadcontainernamesaredistinct-alpha" {
			t.Fatalf("unexpected container name %q", got)
		}
	})
	t.Run("beta", func(t *testing.T) {
		got := uniqueContainerName(t)
		if strings.Contains(got, "alpha") {
			t.Fatalf("beta container name %q leaked the alpha name", got)
		}
	})
}
