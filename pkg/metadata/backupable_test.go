package metadata_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

func TestBackupSentinels_DetectThroughWrap(t *testing.T) {
	sentinels := []struct {
		name string
		err  error
	}{
		{"ErrRestoreDestinationNotEmpty", metadata.ErrRestoreDestinationNotEmpty},
		{"ErrRestoreCorrupt", metadata.ErrRestoreCorrupt},
		{"ErrSchemaVersionMismatch", metadata.ErrSchemaVersionMismatch},
		{"ErrBackupAborted", metadata.ErrBackupAborted},
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

func TestBackupSentinels_NoCrossMatch(t *testing.T) {
	sentinels := []error{
		metadata.ErrRestoreDestinationNotEmpty,
		metadata.ErrRestoreCorrupt,
		metadata.ErrSchemaVersionMismatch,
		metadata.ErrBackupAborted,
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
