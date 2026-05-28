package models

import (
	"errors"
	"fmt"
	"testing"
)

// TestSnapshotErrorSentinels_IsRoundTrip exercises the five Phase-23 (D-23-12)
// orchestration sentinels surfaced to REST in Phase 25. It covers identity,
// wrapped-error round-trip, mutual distinctness, and cross-distinctness with
// the pre-existing Phase-22 snapshot sentinels.
func TestSnapshotErrorSentinels_IsRoundTrip(t *testing.T) {
	phase23 := []struct {
		name string
		err  error
	}{
		{"ErrSnapshotBackupFailed", ErrSnapshotBackupFailed},
		{"ErrSnapshotVerifyFailed", ErrSnapshotVerifyFailed},
		{"ErrSnapshotDrainTimeout", ErrSnapshotDrainTimeout},
		{"ErrSnapshotRetryTargetNotFound", ErrSnapshotRetryTargetNotFound},
		{"ErrSnapshotRetryTargetNotFailed", ErrSnapshotRetryTargetNotFailed},
	}

	// Identity: errors.Is(s, s) is true for each sentinel.
	for _, s := range phase23 {
		t.Run("identity/"+s.name, func(t *testing.T) {
			if !errors.Is(s.err, s.err) {
				t.Fatalf("errors.Is(%s, %s) = false, want true", s.name, s.name)
			}
		})
	}

	// Wrapped: a fmt.Errorf("ctx: %w", s) satisfies errors.Is for the sentinel.
	for _, s := range phase23 {
		t.Run("wrapped/"+s.name, func(t *testing.T) {
			wrapped := fmt.Errorf("create snapshot abc: %w", s.err)
			if !errors.Is(wrapped, s.err) {
				t.Fatalf("errors.Is(wrapped, %s) = false, want true", s.name)
			}
		})
	}

	// Distinctness within Phase 23: no two sentinels alias each other.
	for i, s1 := range phase23 {
		for j, s2 := range phase23 {
			if i == j {
				continue
			}
			t.Run("distinct/"+s1.name+"_vs_"+s2.name, func(t *testing.T) {
				if errors.Is(s1.err, s2.err) {
					t.Fatalf("errors.Is(%s, %s) = true, want false (aliasing)", s1.name, s2.name)
				}
			})
		}
	}

	// Cross-distinctness with Phase-22 sentinels.
	phase22 := []struct {
		name string
		err  error
	}{
		{"ErrSnapshotNotFound", ErrSnapshotNotFound},
		{"ErrSnapshotStateConflict", ErrSnapshotStateConflict},
	}
	for _, s := range phase23 {
		for _, p := range phase22 {
			t.Run("cross/"+s.name+"_vs_"+p.name, func(t *testing.T) {
				if errors.Is(s.err, p.err) {
					t.Fatalf("errors.Is(%s, %s) = true, want false (cross-phase aliasing)", s.name, p.name)
				}
				if errors.Is(p.err, s.err) {
					t.Fatalf("errors.Is(%s, %s) = true, want false (cross-phase aliasing)", p.name, s.name)
				}
			})
		}
	}
}
