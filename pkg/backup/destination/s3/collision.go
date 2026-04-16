package s3

import (
	"context"
	"fmt"
	"strings"

	"github.com/marmos91/dittofs/pkg/backup/destination"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// blockStoreLister is the narrow interface the S3 backup driver uses to
// enforce the D-13 bucket/prefix collision hard-reject against registered
// remote block stores. It is implemented by the composite controlplane
// store, but we depend only on the single method we need so tests can
// supply a local fake without pulling in the entire GORM stack.
type blockStoreLister interface {
	ListBlockStores(ctx context.Context, kind models.BlockStoreKind) ([]*models.BlockStoreConfig, error)
}

// checkPrefixCollision rejects the S3 backup destination when any
// registered remote block store of type "s3" shares the same bucket AND
// has an overlapping prefix (D-13). Block-store GC iterates its configured
// prefix to find orphaned blocks — any overlap could cause GC to
// DeleteObject backup payloads, silently destroying DR capability.
//
// CRITICAL: reads block-store config key "prefix" — the SAME key that
// pkg/controlplane/runtime/shares/service.go:1013 persists. Using any
// other key (e.g. "key_prefix") would read empty every time and silently
// approve the catastrophic root-prefix overlap — the exact PITFALL #8
// this check exists to prevent.
//
// Overlap semantics:
//
//	bucket=X, block_prefix='blocks/',   backup_prefix='metadata/' → OK
//	bucket=X, block_prefix='',          backup_prefix='metadata/' → REJECT (empty-side)
//	bucket=X, block_prefix='data/',     backup_prefix='data/meta/' → REJECT (prefix-of)
//	bucket=X, block_prefix='data/meta', backup_prefix='data/'     → REJECT (superset-of)
//	bucket=Y, any                                                  → OK (different bucket)
func checkPrefixCollision(ctx context.Context, lister blockStoreLister, bucket, backupPrefix string) error {
	stores, err := lister.ListBlockStores(ctx, models.BlockStoreKindRemote)
	if err != nil {
		return fmt.Errorf("%w: list block stores for collision check: %v", destination.ErrIncompatibleConfig, err)
	}
	for _, st := range stores {
		if st == nil || st.Type != "s3" {
			continue
		}
		cfg, err := st.GetConfig()
		if err != nil {
			// Malformed block-store config is the operator's problem to
			// fix, but we conservatively skip it rather than flag a
			// collision on unparseable data — the real block store will
			// fail loud in its own probe.
			continue
		}
		bs, _ := cfg["bucket"].(string)
		if bs != bucket {
			continue
		}
		bp, _ := cfg["prefix"].(string) // <-- real registered block-stores key
		bsPrefix := normalizePrefix(bp)

		a := backupPrefix
		b := bsPrefix
		// Empty prefix on either side means that side covers the whole
		// bucket — always a collision (the catastrophic root-prefix case).
		if a == "" || b == "" {
			return fmt.Errorf("%w: bucket %s has an empty prefix on one side (backup=%q, block=%q from %q) — block-GC could delete backup payloads",
				destination.ErrIncompatibleConfig, bucket, backupPrefix, bsPrefix, st.Name)
		}
		if a == b || strings.HasPrefix(a, b) || strings.HasPrefix(b, a) {
			return fmt.Errorf("%w: bucket %s prefix collision between backup destination %q and block store %q (%q)",
				destination.ErrIncompatibleConfig, bucket, backupPrefix, st.Name, bsPrefix)
		}
	}
	return nil
}
