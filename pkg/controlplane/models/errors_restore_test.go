package models_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

func restoreSentinels() []struct {
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

func TestRestoreSentinels_NonNilAndUnique(t *testing.T) {
	t.Helper()
	sents := restoreSentinels()
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

func TestRestoreSentinels_WrapRoundTrip(t *testing.T) {
	t.Helper()
	for _, s := range restoreSentinels() {
		s := s
		t.Run(s.name, func(t *testing.T) {
			wrapped := fmt.Errorf("restore %q: %w: %v", "snap-1", s.err, errors.New("inner"))
			if !errors.Is(wrapped, s.err) {
				t.Fatalf("errors.Is(wrapped, %s) = false; want true (wrapped = %v)", s.name, wrapped)
			}
		})
	}
}

func TestRestoreSentinels_DistinctFromPriorSnapshotSentinels(t *testing.T) {
	t.Helper()
	prior := []struct {
		name string
		err  error
	}{
		{"ErrSnapshotNotFound", models.ErrSnapshotNotFound},
		{"ErrSnapshotStateConflict", models.ErrSnapshotStateConflict},
		{"ErrSnapshotBackupFailed", models.ErrSnapshotBackupFailed},
		{"ErrSnapshotVerifyFailed", models.ErrSnapshotVerifyFailed},
		{"ErrSnapshotDrainTimeout", models.ErrSnapshotDrainTimeout},
		{"ErrSnapshotRetryTargetNotFound", models.ErrSnapshotRetryTargetNotFound},
		{"ErrSnapshotRetryTargetNotFailed", models.ErrSnapshotRetryTargetNotFailed},
		{"ErrShareNotFound", models.ErrShareNotFound},
		{"ErrDuplicateShare", models.ErrDuplicateShare},
	}
	for _, cur := range restoreSentinels() {
		for _, prev := range prior {
			if errors.Is(cur.err, prev.err) {
				t.Errorf("%s errors.Is %s; want distinct (accidental aliasing)", cur.name, prev.name)
			}
			if errors.Is(prev.err, cur.err) {
				t.Errorf("%s errors.Is %s (reverse); want distinct", prev.name, cur.name)
			}
		}
	}
}
