package snapshots

import (
	"bytes"
	"context"
	"fmt"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/remote"
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
	HashSet *block.HashSet
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

// RunBackup runs a metadata Backup against a counting discard writer and
// returns the dump size + the returned HashSet. The metadata engine must
// implement Snapshotable (memory + badger both do). The harness itself does
// not buffer the dump — the writer discards. Whether the dump is resident
// depends on the engine: badger streams KV-by-KV (only the returned
// HashSet is resident, the quantity the create-path ceiling is built
// around), whereas the memory engine gob-encodes its whole snapshot into
// one internal buffer before writing (so its B/op reflects that buffer,
// not a stream).
func RunBackup(ctx context.Context, store metadata.Store) (BackupResult, error) {
	snapshotable, ok := store.(metadata.Snapshotable)
	if !ok {
		return BackupResult{}, fmt.Errorf("snapshots bench: store is not Snapshotable")
	}
	cd := &countingDiscard{}
	hs, err := snapshotable.WriteSnapshot(ctx, cd)
	if err != nil {
		return BackupResult{}, fmt.Errorf("snapshots bench: backup: %w", err)
	}
	count := 0
	if hs != nil {
		count = hs.Len()
	} else {
		hs = block.NewHashSet(0)
	}
	return BackupResult{DumpBytes: cd.n, ManifestHashes: count, HashSet: hs}, nil
}

// RunWriteManifest serializes hs to a counting discard writer and returns
// the manifest's on-disk byte size. WriteManifest streams sorted hex lines,
// so this measures the sort + encode cost, not a buffer allocation.
func RunWriteManifest(hs *block.HashSet) (int64, error) {
	cd := &countingDiscard{}
	if err := snapshot.WriteManifest(cd, hs); err != nil {
		return 0, fmt.Errorf("snapshots bench: write manifest: %w", err)
	}
	return cd.n, nil
}

// benchLocators maps every hash to a distinct packed-block locator so
// RunVerify's block-only durability probe (#1493) resolves each hash to a
// blocks/<id> object seeded by SeedRemote.
type benchLocators map[block.ContentHash]block.ChunkLocator

func (b benchLocators) GetLocator(_ context.Context, h block.ContentHash) (block.ChunkLocator, bool, error) {
	loc, ok := b[h]
	return loc, ok, nil
}

// blockIDForHash derives a deterministic block ID from a content hash so
// SeedRemote and RunVerify agree on the packed-block key.
func blockIDForHash(h block.ContentHash) string { return "bench-" + h.String() }

// SeedRemote PUTs a one-byte packed block for every hash in hs into rs so a
// subsequent RunVerify finds every block-durability probe present (the
// all-durable happy path, which is also the most expensive: it never
// short-circuits on a miss). Returns the number of objects seeded.
func SeedRemote(ctx context.Context, rs remote.RemoteStore, hs *block.HashSet) (int, error) {
	n := 0
	err := hs.ForEach(func(h block.ContentHash) error {
		if err := rs.PutBlock(ctx, blockIDForHash(h), bytes.NewReader([]byte{0x01})); err != nil {
			return fmt.Errorf("snapshots bench: seed remote put block: %w", err)
		}
		n++
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// RunVerify block-probes every hash in hs against rs at VerifyConcurrency
// and returns the number of hashes probed. The caller times the call. With
// an all-present remote (see SeedRemote) every probe runs, giving the
// worst-case verify wall time for the manifest size.
func RunVerify(ctx context.Context, rs remote.RemoteStore, hs *block.HashSet) (int, error) {
	locators := make(benchLocators, hs.Len())
	_ = hs.ForEach(func(h block.ContentHash) error {
		locators[h] = block.ChunkLocator{BlockID: blockIDForHash(h)}
		return nil
	})
	if err := snapshot.VerifyRemoteDurability(ctx, locators, rs, hs, VerifyConcurrency); err != nil {
		return 0, fmt.Errorf("snapshots bench: verify: %w", err)
	}
	return hs.Len(), nil
}

// SerializeManifest renders hs to its on-disk byte form via the production
// WriteManifest encoder. Benchmarks call it once, outside the timed loop,
// then feed the bytes to RunReadManifest so only the parse is measured.
func SerializeManifest(hs *block.HashSet) ([]byte, error) {
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
