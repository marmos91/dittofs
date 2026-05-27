package metadata_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

func TestErrRestoreDestinationNotEmpty_DetectsThroughWrap(t *testing.T) {
	wrapped := fmt.Errorf("outer: %w", metadata.ErrRestoreDestinationNotEmpty)
	if !errors.Is(wrapped, metadata.ErrRestoreDestinationNotEmpty) {
		t.Fatalf("errors.Is failed to detect ErrRestoreDestinationNotEmpty through wrapping")
	}
}

func TestErrRestoreCorrupt_DetectsThroughWrap(t *testing.T) {
	wrapped := fmt.Errorf("outer: %w", metadata.ErrRestoreCorrupt)
	if !errors.Is(wrapped, metadata.ErrRestoreCorrupt) {
		t.Fatalf("errors.Is failed to detect ErrRestoreCorrupt through wrapping")
	}
}

func TestErrSchemaVersionMismatch_DetectsThroughWrap(t *testing.T) {
	wrapped := fmt.Errorf("outer: %w", metadata.ErrSchemaVersionMismatch)
	if !errors.Is(wrapped, metadata.ErrSchemaVersionMismatch) {
		t.Fatalf("errors.Is failed to detect ErrSchemaVersionMismatch through wrapping")
	}
}

func TestErrBackupAborted_DetectsThroughWrap(t *testing.T) {
	wrapped := fmt.Errorf("outer: %w", metadata.ErrBackupAborted)
	if !errors.Is(wrapped, metadata.ErrBackupAborted) {
		t.Fatalf("errors.Is failed to detect ErrBackupAborted through wrapping")
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
