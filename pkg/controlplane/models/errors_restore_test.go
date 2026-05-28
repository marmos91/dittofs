package models_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// phase24Sentinels returns the seven new sentinels added in D-24-08.
// Centralized so each subtest iterates the same canonical set.
func phase24Sentinels() []struct {
	name string
	err  error
} {
	return []struct {
		name string
		err  error
	}{
		{"ErrShareEnabled", models.ErrShareEnabled},
		{"ErrSnapshotNotDurable", models.ErrSnapshotNotDurable},
		{"ErrSnapshotMetadataDumpMissing", models.ErrSnapshotMetadataDumpMissing},
		{"ErrMetadataStoreNotResetable", models.ErrMetadataStoreNotResetable},
		{"ErrRestoreSafetySnapFailed", models.ErrRestoreSafetySnapFailed},
		{"ErrRestoreAborted", models.ErrRestoreAborted},
		{"ErrRestoreVerifyFailed", models.ErrRestoreVerifyFailed},
	}
}

// TestPhase24Sentinels_NonNilAndUnique asserts every Phase 24 sentinel
// is non-nil and each carries a unique message string.
func TestPhase24Sentinels_NonNilAndUnique(t *testing.T) {
	t.Helper()
	sents := phase24Sentinels()
	seen := make(map[string]string, len(sents))
	for _, s := range sents {
		if s.err == nil {
			t.Errorf("sentinel %s is nil; want non-nil", s.name)
			continue
		}
		msg := s.err.Error()
		if msg == "" {
			t.Errorf("sentinel %s has empty message", s.name)
		}
		if prev, ok := seen[msg]; ok {
			t.Errorf("sentinel %s shares message %q with %s; messages must be unique", s.name, msg, prev)
		}
		seen[msg] = s.name
	}
}

// TestPhase24Sentinels_WrapRoundTrip asserts each sentinel survives a
// fmt.Errorf("%w") wrapping layer used by Runtime orchestration.
func TestPhase24Sentinels_WrapRoundTrip(t *testing.T) {
	t.Helper()
	for _, s := range phase24Sentinels() {
		s := s
		t.Run(s.name, func(t *testing.T) {
			wrapped := fmt.Errorf("restore %q: %w: %v", "snap-1", s.err, errors.New("inner"))
			if !errors.Is(wrapped, s.err) {
				t.Fatalf("errors.Is(wrapped, %s) = false; want true (wrapped = %v)", s.name, wrapped)
			}
		})
	}
}

// TestPhase24Sentinels_NoCollisionWithPriorPhases asserts no Phase 24
// sentinel is errors.Is-equal to any prior-phase sentinel (Phase 22 + 23).
func TestPhase24Sentinels_NoCollisionWithPriorPhases(t *testing.T) {
	t.Helper()
	prior := []struct {
		name string
		err  error
	}{
		// Phase 22.
		{"ErrSnapshotNotFound", models.ErrSnapshotNotFound},
		{"ErrSnapshotStateConflict", models.ErrSnapshotStateConflict},
		// Phase 23.
		{"ErrSnapshotBackupFailed", models.ErrSnapshotBackupFailed},
		{"ErrSnapshotVerifyFailed", models.ErrSnapshotVerifyFailed},
		{"ErrSnapshotDrainTimeout", models.ErrSnapshotDrainTimeout},
		{"ErrSnapshotRetryTargetNotFound", models.ErrSnapshotRetryTargetNotFound},
		{"ErrSnapshotRetryTargetNotFailed", models.ErrSnapshotRetryTargetNotFailed},
		// Long-standing share sentinels.
		{"ErrShareNotFound", models.ErrShareNotFound},
		{"ErrDuplicateShare", models.ErrDuplicateShare},
	}
	for _, p24 := range phase24Sentinels() {
		for _, prev := range prior {
			if errors.Is(p24.err, prev.err) {
				t.Errorf("%s errors.Is %s; want distinct (accidental aliasing)", p24.name, prev.name)
			}
			if errors.Is(prev.err, p24.err) {
				t.Errorf("%s errors.Is %s (reverse); want distinct", prev.name, p24.name)
			}
		}
	}
}
