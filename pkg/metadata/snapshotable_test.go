package metadata_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

func TestSnapshotSentinels_DetectThroughWrap(t *testing.T) {
	sentinels := []struct {
		name string
		err  error
	}{
		{"ErrRestoreDestinationNotEmpty", metadata.ErrRestoreDestinationNotEmpty},
		{"ErrRestoreCorrupt", metadata.ErrRestoreCorrupt},
		{"ErrSchemaVersionMismatch", metadata.ErrSchemaVersionMismatch},
		{"ErrSnapshotAborted", metadata.ErrSnapshotAborted},
	}
	for _, tc := range sentinels {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := fmt.Errorf("outer: %w", tc.err)
			if !errors.Is(wrapped, tc.err) {
				t.Fatalf("errors.Is failed to detect %s through wrapping", tc.name)
			}
		})
	}
}

func TestSnapshotSentinels_NoCrossMatch(t *testing.T) {
	sentinels := []error{
		metadata.ErrRestoreDestinationNotEmpty,
		metadata.ErrRestoreCorrupt,
		metadata.ErrSchemaVersionMismatch,
		metadata.ErrSnapshotAborted,
	}
	for i, a := range sentinels {
		for j, b := range sentinels {
			if i == j {
				continue
			}
			if errors.Is(a, b) {
				t.Fatalf("sentinel %d (%v) unexpectedly matches sentinel %d (%v)", i, a, j, b)
			}
		}
	}
}
