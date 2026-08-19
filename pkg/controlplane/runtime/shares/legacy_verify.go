package shares

// legacy_verify.go decides whether a pre-journal migration may believe itself.
//
// Archiving blobs/+logs/ aside and seeding cold intervals from the manifest is
// bookkeeping: it makes the byte *counts* right. Whether a read returns the
// stored bytes or a zero-fill is a separate question, and answering it wrongly
// is invisible — the file is the right length and the wrong content. So the
// migration reads a sample back and hashes it before it reports success, and it
// only deletes the archive it made once that sample proves the share can serve
// its own data from the remote.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"time"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/local"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// coldVerifySamples is how many extents the migration reads back. The failure
// it guards against — a cold range that comes back as a hole — is systemic
// rather than per-file, so a handful of extents is enough to expose it; the
// point is to have read *something* before claiming success.
const coldVerifySamples = 8

// migrationProgressInterval is how often a migration loop whose cost scales with
// the data reports what it has done so far: long enough that a small store logs
// nothing extra, short enough that a large one never looks wedged.
const migrationProgressInterval = 5 * time.Second

// coldSample is one manifest extent the migration will read back and hash.
type coldSample struct {
	payloadID string
	offset    int64
	length    int64
	hash      block.ContentHash
}

// coldSeedReport is what a manifest seed learned on the way through.
type coldSeedReport struct {
	// payloads and chunks are what the manifest described.
	payloads int
	chunks   int

	// unsynced counts chunks the manifest does not yet call remote. Their only
	// copy is in the archived pre-journal layout, so an archive cannot be
	// deleted while this is non-zero.
	unsynced int

	// unplaceable counts manifest rows whose ID carries no parseable offset. Which
	// bytes they describe is not recoverable from the row, so nothing could be
	// seeded for them and no later read can serve them. The archive may hold the
	// only copy that is still placeable, so this is weighed alongside unsynced
	// when deciding whether to reap.
	unplaceable int

	// samples are extents to read back, capped at coldVerifySamples.
	samples []coldSample
}

// sampledPayload reports whether payloadID has no sample yet, so the sample set
// spreads over distinct files instead of filling up with one file's chunks.
// Payloads are enumerated one at a time, so only the newest sample can match.
func (r *coldSeedReport) sampledPayload(payloadID string) bool {
	return len(r.samples) == 0 || r.samples[len(r.samples)-1].payloadID != payloadID
}

// errColdVerifyUnavailable marks a verification that could not be carried out —
// the remote was unreachable, not wrong. Nothing is proven either way, so the
// caller keeps the archive and serves the share.
var errColdVerifyUnavailable = errors.New("cold verification unavailable")

// verifyColdSeed reads each sampled extent back through the block store and
// compares its BLAKE3 against the manifest. A mismatch is proof the share
// cannot serve its own data: the read either zero-filled a range it should have
// fetched or the remote holds something else. A read *error* proves nothing —
// the remote may simply be unreachable — and comes back wrapped so the caller
// can tell "wrong" from "unknown".
func verifyColdSeed(ctx context.Context, bs *engine.Store, report coldSeedReport) error {
	for _, s := range report.samples {
		buf := make([]byte, s.length)
		n, err := bs.ReadAt(ctx, s.payloadID, buf, uint64(s.offset))
		if err != nil {
			return fmt.Errorf("%w: read back %s at %d: %w", errColdVerifyUnavailable, s.payloadID, s.offset, err)
		}
		if int64(n) != s.length {
			return fmt.Errorf("read back %s at %d: got %d bytes, manifest says %d",
				s.payloadID, s.offset, n, s.length)
		}
		if got := block.ContentHash(blake3.Sum256(buf)); got != s.hash {
			return fmt.Errorf("read back %s at %d (%d bytes): content hash %s, manifest says %s",
				s.payloadID, s.offset, s.length, got, s.hash)
		}
	}
	return nil
}

// finishLegacyArchiveMigration is the last step of a remote-backed pre-journal
// migration: prove the share can serve its own data, then take back the disk the
// migration was holding.
//
// Three outcomes, in decreasing order of confidence:
//
//   - Verified, and every chunk is remote: the archive is redundant, so delete
//     it. This is the case that gave the disk back on the deployment that hit
//     the hole bug — 55 GiB an operator had no way to know was safe to remove.
//   - Verified, but some chunks never reached the remote: those bytes exist
//     nowhere else, so keep the archive and name it in the log.
//   - The sample did not match the manifest: refuse to serve the share. The
//     archive and the remote are both untouched, so this is recoverable, and
//     the alternative is handing out zeros that look like real short reads.
//     A sample that could not be *read* proves nothing — the remote may be
//     unreachable — so that only warns.
func finishLegacyArchiveMigration(
	ctx context.Context,
	bs *engine.Store,
	m legacyArchiveMigrator,
	report coldSeedReport,
	shareName string,
) error {
	archives := m.LegacyArchivePaths()
	verr := verifyColdSeed(ctx, bs, report)
	switch {
	case errors.Is(verr, errColdVerifyUnavailable):
		logger.Warn("migrated pre-journal local layout but could not read a sample back to verify it; "+
			"keeping the archived blobs/+logs/ until a later start verifies them",
			"share", shareName, "payloads", report.payloads, "chunks", report.chunks,
			"archive", archives, "error", verr)
		return nil
	case verr != nil:
		return fmt.Errorf("pre-journal migration of share %q cannot serve its own data (%w); "+
			"the archived pre-journal bytes are intact at %v and the remote store is untouched, "+
			"so refusing to serve zeros — restore the archive or investigate before retrying",
			shareName, verr, archives)
	}
	if report.unsynced > 0 || report.unplaceable > 0 {
		logger.Warn("migrated pre-journal local layout and verified a sample, but some chunks are not accounted for; "+
			"keeping the archived blobs/+logs/ because those bytes may exist nowhere else",
			"share", shareName, "payloads", report.payloads, "chunks", report.chunks,
			"chunks_not_remote", report.unsynced, "chunks_unplaceable", report.unplaceable,
			"archive", archives)
		return nil
	}
	freed := archiveBytes(archives)
	if err := m.DiscardLegacyArchive(); err != nil {
		// The migration itself succeeded; only the cleanup did not. Say so and
		// serve the share — a stale archive costs disk, not correctness.
		logger.Warn("migrated pre-journal local layout and verified it, but deleting the archived blobs/+logs/ failed; "+
			"they are safe to remove by hand",
			"share", shareName, "archive", archives, "error", err)
		return nil
	}
	logger.Info("migrated pre-journal local layout: seeded cold intervals from manifest, verified a sample reads back, "+
		"and reclaimed the archived blobs/+logs/",
		"share", shareName, "payloads", report.payloads, "chunks", report.chunks,
		"samples_verified", len(report.samples), "bytes_reclaimed", freed)
	return nil
}

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
	m, isMigrator := localStore.(legacyArchiveMigrator)
	migrated := isMigrator && m.MigratedFromLegacy()
	if !migrated && tracker.ColdSeeded() {
		return nil
	}
	metaStore, ok := fileChunkStore.(metadata.Store)
	if !ok {
		logger.Warn("local tier has no record of a manifest seed but the metadata store cannot enumerate one; "+
			"ranges held only by the remote will read as zeros",
			"share", shareName)
		return nil
	}
	report, err := SeedColdFromManifest(ctx, bs, metaStore)
	if err != nil {
		return fmt.Errorf("seed cold intervals from manifest: %w", err)
	}
	if migrated {
		if err := finishLegacyArchiveMigration(ctx, bs, m, report, shareName); err != nil {
			return err
		}
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

// legacyArchiveMigrator is the local-store surface driven after a remote-backed
// pre-journal migration: it reports that the migration happened, and it can
// delete the archive that migration made once the data is shown to be readable.
// Implemented by the journal-backed fs store; other backends never satisfy it,
// so the whole block is a no-op for them.
type legacyArchiveMigrator interface {
	MigratedFromLegacy() bool
	LegacyArchivePaths() []string
	DiscardLegacyArchive() error
}

// dirBytes totals the file sizes under path, for a log line that tells the
// operator how much the reap gave back. Best-effort: an unreadable entry is
// skipped rather than failing a cleanup that has already been decided.
func dirBytes(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // a partial total still beats no total
		}
		if info, ierr := d.Info(); ierr == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// archiveBytes totals every archive directory that still exists.
func archiveBytes(paths []string) int64 {
	var total int64
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		total += dirBytes(p)
	}
	return total
}
