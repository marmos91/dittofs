package shares

// legacy_verify.go seeds a local tier's cold intervals from the metadata
// manifest.
//
// A journal that opened empty over data that lives on the remote holds no
// interval for those ranges, which makes them indistinguishable from POSIX
// holes: a read zero-fills, does not fetch, and reports no error. Seeding
// arms the cold fetch so the bytes come back from the remote instead.

import (
	"context"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/local"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// migrationProgressInterval is how often a seed loop whose cost scales with the
// data reports what it has done so far: long enough that a small store logs
// nothing extra, short enough that a large one never looks wedged.
const migrationProgressInterval = 5 * time.Second

// coldSeedBatchExtents is how many extents SeedColdFromManifest buffers before
// making them durable. Each flush is one fsync, so a bigger batch is strictly
// faster and strictly more heap; 64Ki extents is a few MiB of entries and takes
// the measured 56k-chunk manifest in a single write.
const coldSeedBatchExtents = 64 << 10

// seedColdIfNeeded gives a local tier the cold intervals its manifest says it is
// missing, once per local tier.
//
// A journal that opened empty over data that lives on the remote — the open that
// archived a pre-journal layout aside, or any earlier open that did the same
// under a build that only seeded in memory — holds no interval for those ranges.
// They are then indistinguishable from POSIX holes: a read zero-fills, does not
// fetch, and reports no error. So the trigger is not "this open migrated
// something" (a one-shot the affected stores have already spent) but "this local
// tier has never been seeded", which stays true until a seed finishes.
//
// The marker is written last. An interrupted seed leaves none and repeats on the
// next open, which costs a rescan and nothing else: seeding skips the ranges the
// journal already covers, and the remote bytes and the manifest are untouched
// throughout.
//
// Remote-backed shares only. Seeding a cold interval on a share with no remote
// would point a read at a store that cannot serve it.
//
// ponytail: O(files) manifest scan, once per local tier; a lazy per-read seed is
// the upgrade path if this ever bites a share with a huge file count.
func seedColdIfNeeded(
	ctx context.Context,
	bs *engine.Store,
	localStore local.LocalStore,
	fileChunkStore block.EngineFileChunkStore,
	remoteConfigured bool,
	shareName string,
) error {
	if !remoteConfigured {
		return nil
	}
	tracker, ok := localStore.(coldSeedTracker)
	if !ok {
		return nil
	}
	if tracker.ColdSeeded() {
		return nil
	}
	metaStore, ok := fileChunkStore.(metadata.Store)
	if !ok {
		logger.Warn("local tier has no record of a manifest seed but the metadata store cannot enumerate one; "+
			"ranges held only by the remote will read as zeros",
			"share", shareName)
		return nil
	}
	if err := SeedColdFromManifest(ctx, bs, metaStore); err != nil {
		return fmt.Errorf("seed cold intervals from manifest: %w", err)
	}
	if err := tracker.MarkColdSeeded(); err != nil {
		// The seed itself is durable; only the record of it is missing, so the
		// next open pays for the scan again and reaches the same state.
		logger.Warn("seeded cold intervals from the manifest but could not record it; the next start will seed again",
			"share", shareName, "error", err)
	}
	return nil
}

// coldSeedTracker is the local-store surface that remembers, across restarts,
// whether this local tier has been seeded from the metadata manifest.
// Implemented by the journal-backed fs store; other backends never satisfy it,
// so seeding is skipped for them entirely.
type coldSeedTracker interface {
	ColdSeeded() bool
	MarkColdSeeded() error
}
