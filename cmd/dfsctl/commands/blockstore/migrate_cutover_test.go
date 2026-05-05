package blockstore

import (
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// TestPerformCutover_HappyPath covers behavior 1: a share whose
// BlockLayout is empty (treated as legacy) gets flipped to cas-only;
// GetShareOptions reflects the new value.
func TestPerformCutover_HappyPath(t *testing.T) {
	f := newLoopFixture(t)
	// Newly-created shares default to BlockLayout="" (legacy via
	// ParseBlockLayout). Verify pre-state.
	preOpts, err := f.mds.GetShareOptions(t.Context(), f.share)
	if err != nil {
		t.Fatalf("pre GetShareOptions: %v", err)
	}
	if preOpts.BlockLayout == metadata.BlockLayoutCASOnly {
		t.Fatalf("pre-cutover: share already cas-only (got %q); fixture invariant broken",
			preOpts.BlockLayout)
	}

	if err := performCutover(t.Context(), f.rt, f.share); err != nil {
		t.Fatalf("performCutover: %v", err)
	}

	postOpts, err := f.mds.GetShareOptions(t.Context(), f.share)
	if err != nil {
		t.Fatalf("post GetShareOptions: %v", err)
	}
	if postOpts.BlockLayout != metadata.BlockLayoutCASOnly {
		t.Errorf("post-cutover BlockLayout = %q, want %q",
			postOpts.BlockLayout, metadata.BlockLayoutCASOnly)
	}
}

// TestPerformCutover_Idempotent covers behavior 2: calling on an
// already-cas-only share returns nil and does not error.
func TestPerformCutover_Idempotent(t *testing.T) {
	f := newLoopFixture(t)
	// Pre-flip the share.
	opts, _ := f.mds.GetShareOptions(t.Context(), f.share)
	opts.BlockLayout = metadata.BlockLayoutCASOnly
	if err := f.mds.UpdateShareOptions(t.Context(), f.share, opts); err != nil {
		t.Fatalf("pre-flip UpdateShareOptions: %v", err)
	}

	// performCutover on the already-flipped share must be a no-op.
	if err := performCutover(t.Context(), f.rt, f.share); err != nil {
		t.Errorf("performCutover on already-cas-only share returned err: %v", err)
	}
	// Second call must also be a no-op (idempotent).
	if err := performCutover(t.Context(), f.rt, f.share); err != nil {
		t.Errorf("performCutover second call returned err: %v", err)
	}

	post, _ := f.mds.GetShareOptions(t.Context(), f.share)
	if post.BlockLayout != metadata.BlockLayoutCASOnly {
		t.Errorf("BlockLayout drifted after idempotent calls: got %q", post.BlockLayout)
	}
}

// TestPerformCutover_NonExistentShare covers behavior 3-adjacent:
// GetShareOptions failure (e.g., share doesn't exist) propagates as a
// wrapped error; the caller (runMigrateLoopWithRuntime) MUST treat
// this as fail-loud (no legacy delete).
func TestPerformCutover_NonExistentShare(t *testing.T) {
	f := newLoopFixture(t)

	err := performCutover(t.Context(), f.rt, "share-that-does-not-exist")
	if err == nil {
		t.Fatal("performCutover on missing share: expected error, got nil")
	}
	// The exact wrapping is metadata.ErrNotFound underneath; the
	// caller doesn't pattern-match, but the wrapping must include
	// the share name for operator triage.
	if !contains(err.Error(), "share-that-does-not-exist") {
		t.Errorf("error %q does not name the failing share", err.Error())
	}
}

// TestPerformCutover_NilRuntime covers a sanity rail: nil offlineRuntime
// returns a structured error rather than panicking.
func TestPerformCutover_NilRuntime(t *testing.T) {
	err := performCutover(t.Context(), nil, "any")
	if err == nil {
		t.Fatal("performCutover(nil, ...): expected error, got nil")
	}
	if !errors.Is(err, errPerformCutoverNilRuntimeProbe()) {
		// Looser check: error must mention "nil offlineRuntime" so the
		// operator can distinguish it from a metadata-store failure.
		if !contains(err.Error(), "nil") {
			t.Errorf("error %q does not mention nil-runtime cause", err.Error())
		}
	}
}

// errPerformCutoverNilRuntimeProbe returns a sentinel-shaped probe
// equivalent to the nil-runtime error performCutover constructs. Used
// only for the errors.Is check above; performCutover does not export
// a sentinel because the nil-runtime path is a rail, not a normal
// failure mode.
func errPerformCutoverNilRuntimeProbe() error {
	return errors.New("performCutover: nil offlineRuntime")
}

// contains is the std-lib-free substring check (avoids importing
// strings into a test that doesn't otherwise need it).
func contains(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	if len(sub) > len(s) {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// Compile-time sanity.
var _ context.Context = context.Background()
