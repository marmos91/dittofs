package journal

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestSweepIdxSidecars_OnlyUnlinksOwnSidecars pins the sweep to the names this
// store could itself have written. A sidecar is <segment-id>.idx; anything else
// sharing the suffix belongs to another writer, and unlinking it would make the
// journal directory unsafe to share with one.
func TestSweepIdxSidecars_OnlyUnlinksOwnSidecars(t *testing.T) {
	dir := t.TempDir()

	swept := []string{
		fmt.Sprintf(segIDFmt+idxSuffix, 7),
		fmt.Sprintf(segIDFmt+idxSuffix, 0),
	}
	kept := []string{
		"notanumber" + idxSuffix,
		"12ab" + idxSuffix,
		"-3" + idxSuffix,
		// Parses as an ID but is not a name segPath would ever produce.
		"7" + idxSuffix,
		fmt.Sprintf(segIDFmt+segSuffix, 9),
		idxSuffix,
	}
	for _, n := range append(append([]string{}, swept...), kept...) {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatalf("seed %s: %v", n, err)
		}
	}

	(&Store{dir: dir}).sweepIdxSidecars()

	for _, n := range swept {
		if _, err := os.Stat(filepath.Join(dir, n)); !os.IsNotExist(err) {
			t.Errorf("%s: want unlinked, stat err = %v", n, err)
		}
	}
	for _, n := range kept {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Errorf("%s: want kept, stat err = %v", n, err)
		}
	}
}
