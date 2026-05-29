package snapshots

import (
	"bytes"
	"context"
	"fmt"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/remote"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/snapshot"
)

// VerifyConcurrency mirrors the hardcoded production verify fan-out
// (runtime/snapshot.go). The benchmarks probe at the same width so the
// measured per-block latency budget transfers directly.
const VerifyConcurrency = 16

// BackupResult reports the cost of one Backup pass.
type BackupResult struct {
	// DumpBytes is the number of bytes streamed to the dump writer.
	DumpBytes int64

	// ManifestHashes is the number of unique block hashes the engine
	// returned in the HashSet (= manifest line count).
	ManifestHashes int

	// HashSet is the engine-returned set, reused by the manifest + verify
	// workloads so a benchmark can chain them without re-running Backup.
	HashSet *blockstore.HashSet
}

// countingDiscard counts bytes written to it and drops them. Used so the
// dump stream is fully serialized (exercising the engine's per-KV write
// path) without allocating an O(dump) buffer — the dump is meant to be
// streamed, and the benchmark's RAM ceiling should reflect that.
type countingDiscard struct{ n int64 }

func (c *countingDiscard) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// RunBackup streams a metadata Backup to a counting discard writer and
// returns the dump size + the returned HashSet. The metadata engine must
// implement Backupable (memory + badger both do). The dump is NOT buffered
// in RAM here; only the HashSet the engine accumulates is resident, which
// is the quantity the create-path memory ceiling is built around.
func RunBackup(ctx context.Context, store metadata.MetadataStore) (BackupResult, error) {
	backupable, ok := store.(metadata.Backupable)
	if !ok {
		return BackupResult{}, fmt.Errorf("snapshots bench: store is not Backupable")
	}
	cd := &countingDiscard{}
	hs, err := backupable.Backup(ctx, cd)
	if err != nil {
		return BackupResult{}, fmt.Errorf("snapshots bench: backup: %w", err)
	}
	count := 0
	if hs != nil {
		count = hs.Len()
	} else {
		hs = blockstore.NewHashSet(0)
	}
	return BackupResult{DumpBytes: cd.n, ManifestHashes: count, HashSet: hs}, nil
}

// RunWriteManifest serializes hs to a counting discard writer and returns
// the manifest's on-disk byte size. WriteManifest streams sorted hex lines,
// so this measures the sort + encode cost, not a buffer allocation.
func RunWriteManifest(hs *blockstore.HashSet) (int64, error) {
	cd := &countingDiscard{}
	if err := snapshot.WriteManifest(cd, hs); err != nil {
		return 0, fmt.Errorf("snapshots bench: write manifest: %w", err)
	}
	return cd.n, nil
}

// SeedRemote PUTs a zero-length object for every hash in hs into rs so a
// subsequent RunVerify finds every probe present (the all-durable happy
// path, which is also the most expensive: it never short-circuits on a
// miss). Returns the number of objects seeded.
func SeedRemote(ctx context.Context, rs remote.RemoteStore, hs *blockstore.HashSet) (int, error) {
	n := 0
	err := hs.ForEach(func(h blockstore.ContentHash) error {
		if err := rs.Put(ctx, h, nil); err != nil {
			return fmt.Errorf("snapshots bench: seed remote put: %w", err)
		}
		n++
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// RunVerify HEAD-probes every hash in hs against rs at VerifyConcurrency
// and returns the number of hashes probed. The caller times the call. With
// an all-present remote (see SeedRemote) every probe runs, giving the
// worst-case verify wall time for the manifest size.
func RunVerify(ctx context.Context, rs remote.RemoteStore, hs *blockstore.HashSet) (int, error) {
	if err := snapshot.VerifyRemoteDurability(ctx, rs, hs, VerifyConcurrency); err != nil {
		return 0, fmt.Errorf("snapshots bench: verify: %w", err)
	}
	return hs.Len(), nil
}

// SerializeManifest renders hs to its on-disk byte form via the production
// WriteManifest encoder. Benchmarks call it once, outside the timed loop,
// then feed the bytes to RunReadManifest so only the parse is measured.
func SerializeManifest(hs *blockstore.HashSet) ([]byte, error) {
	var buf bytes.Buffer
	if err := snapshot.WriteManifest(&buf, hs); err != nil {
		return nil, fmt.Errorf("snapshots bench: serialize manifest: %w", err)
	}
	return buf.Bytes(), nil
}

// RunReadManifest parses a serialized manifest (see SerializeManifest) back
// into a HashSet, measuring the ReadManifest cost — the dominant allocation
// on the restore pre-verify path, where the parsed set is resident in RAM.
// Returns the parsed hash count.
func RunReadManifest(manifest []byte) (int, error) {
	parsed, err := snapshot.ReadManifest(bytes.NewReader(manifest))
	if err != nil {
		return 0, fmt.Errorf("snapshots bench: read manifest: %w", err)
	}
	return parsed.Len(), nil
}
