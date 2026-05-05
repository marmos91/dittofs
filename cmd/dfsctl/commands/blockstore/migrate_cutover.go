package blockstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// performCutover flips the share's BlockLayout from legacy to cas-only
// via metadataStore.UpdateShareOptions. Idempotent: safe to call on a
// share that is already cas-only — returns nil with a debug log.
//
// Strict ordering invariant (D-A7, D-A13, T-14-05-02): callers MUST
// have run verifyIntegrity to success before invoking this. The
// integrity → cutover → legacy delete pipeline is enforced by
// runMigrateLoopWithRuntime; performCutover does not re-check.
//
// Failure semantic (D-A8 fail-loud): if UpdateShareOptions errors, the
// caller MUST short-circuit and not invoke deleteLegacyKeys — the
// share's BlockLayout is still legacy, the dual-read shim is still
// authoritative, and removing legacy keys would corrupt the live data
// path. The wiring in runMigrateLoopWithRuntime enforces this via early
// return on a non-nil performCutover error.
func performCutover(ctx context.Context, svc *offlineRuntime, share string) error {
	if svc == nil {
		return errors.New("performCutover: nil offlineRuntime")
	}
	if svc.MetadataStore() == nil {
		return errors.New("performCutover: nil metadata store")
	}

	opts, err := svc.MetadataStore().GetShareOptions(ctx, share)
	if err != nil {
		return fmt.Errorf("performCutover: read share options for %q: %w", share, err)
	}
	if opts == nil {
		return fmt.Errorf("performCutover: nil ShareOptions for share %q", share)
	}

	if opts.BlockLayout == metadata.BlockLayoutCASOnly {
		logger.Info("blockstore migrate: share already cas-only; cutover is a no-op",
			"share", share)
		return nil
	}

	opts.BlockLayout = metadata.BlockLayoutCASOnly
	if err := svc.MetadataStore().UpdateShareOptions(ctx, share, opts); err != nil {
		return fmt.Errorf("performCutover: flip block_layout to cas-only for share %q: %w",
			share, err)
	}
	logger.Info("blockstore migrate: share block_layout flipped to cas-only",
		"share", share)
	return nil
}
