// Package storebackups — backup_hold.go implements the Phase-5 SAFETY-01
// block-GC retention hold. See pkg/blockstore/gc.BackupHoldProvider and
// .planning/phases/05-restore-orchestration-safety-rails/05-CONTEXT.md D-11.
package storebackups

import (
	"context"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/blockstore/gc"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// BackupHold implements gc.BackupHoldProvider by unioning PayloadIDSet
// fields from every succeeded BackupRecord's manifest across every
// registered repo.
//
// Per-repo and per-record errors are logged and skipped (continue-on-
// error), matching the Phase-4 retention pattern (D-13). Better to
// under-hold slightly than fail the whole GC run.
//
// See Phase 5 CONTEXT.md D-11, D-12.
type BackupHold struct {
	store       store.BackupStore
	destFactory DestinationFactoryFn
}

// NewBackupHold constructs a BackupHold. destFactory is typically the
// same factory used by Service.RunBackup — passing the same instance
// keeps destination-lifecycle semantics identical between backup and
// GC-hold paths.
func NewBackupHold(backupStore store.BackupStore, destFactory DestinationFactoryFn) *BackupHold {
	return &BackupHold{store: backupStore, destFactory: destFactory}
}

// HeldPayloadIDs implements gc.BackupHoldProvider. It iterates every repo,
// every succeeded record, fetches manifest.yaml via Destination.GetManifestOnly
// (cheap — ~KB per fetch; no payload bandwidth), and unions every
// manifest.PayloadIDSet into a single held set.
//
// Error handling is continue-on-error at two nested layers (D-13):
//   - Per-repo: destFactory or ListSucceededRecordsByRepo failures log WARN
//     and skip the repo. GC under-holds for that repo rather than failing.
//   - Per-record: GetManifestOnly failures log WARN and skip the record. GC
//     under-holds for that record rather than failing.
//
// Only ListAllBackupRepos failures are returned to the caller — without a
// repo list there is nothing to iterate, and continuing would silently hide
// an infrastructure-level outage from GC.
//
// Always returns a non-nil map (possibly empty).
func (h *BackupHold) HeldPayloadIDs(ctx context.Context) (map[metadata.PayloadID]struct{}, error) {
	out := make(map[metadata.PayloadID]struct{})

	repos, err := h.store.ListAllBackupRepos(ctx)
	if err != nil {
		return nil, err
	}

	for _, repo := range repos {
		dst, err := h.destFactory(ctx, repo)
		if err != nil {
			logger.Warn("BackupHold: skip repo on destFactory error",
				"repo_id", repo.ID, "error", err)
			continue
		}

		records, err := h.store.ListSucceededRecordsByRepo(ctx, repo.ID)
		if err != nil {
			_ = dst.Close()
			logger.Warn("BackupHold: skip repo on list error",
				"repo_id", repo.ID, "error", err)
			continue
		}

		for _, rec := range records {
			m, err := dst.GetManifestOnly(ctx, rec.ID)
			if err != nil {
				logger.Warn("BackupHold: skip record on manifest fetch error",
					"repo_id", repo.ID, "record_id", rec.ID, "error", err)
				continue
			}
			for _, pid := range m.PayloadIDSet {
				out[metadata.PayloadID(pid)] = struct{}{}
			}
		}

		_ = dst.Close()
	}

	return out, nil
}

// Compile-time check: BackupHold satisfies gc.BackupHoldProvider.
var _ gc.BackupHoldProvider = (*BackupHold)(nil)
