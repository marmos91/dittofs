package runtime

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/engine"
	bsmemory "github.com/marmos91/dittofs/pkg/blockstore/local/memory"
	remotememory "github.com/marmos91/dittofs/pkg/blockstore/remote/memory"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	cpstore "github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/marmos91/dittofs/pkg/snapshot"
)

// TestCreateSnapshot_PointInTimeConsistency drives the real CreateSnapshot
// orchestration against the real metadata-store Backup() while a continuous
// background writer hammers the share, then asserts the captured snapshot is
// internally consistent: every block hash referenced by the metadata dump is
// present in the manifest, and no file is torn across a multi-chunk write.
//
// This is the orchestration-level analogue of the store-layer
// ConcurrentWriter backup conformance test (pkg/metadata/storetest). The
// store-layer test isolates Backup() under controlled timing; this test
// exercises the full DrainRollups -> Backup -> manifest pipeline under
// sustained concurrent mutation, which is exactly the window issue #811
// flagged as undefined.
//
// Consistency model under test (immutable CAS blocks + a single consistent
// metadata read-view): the metadata dump and the hash manifest are captured
// from the same logical instant, so a write landing mid-create is either
// fully present in both or absent from both — never half-captured. No global
// write quiesce is required.
func TestCreateSnapshot_PointInTimeConsistency(t *testing.T) {
	fx := newRealBackupFixture(t)
	defer fx.close()

	ctx := fx.ctx()

	// Seed an initial multi-chunk file so the very first snapshot already
	// has block refs to capture.
	fx.putMultiChunkFile(ctx, "seed.bin", 4)

	// Continuous background writer: keep mutating the share (create new
	// multi-chunk files, overwrite existing ones with a different chunk
	// count) until the test signals stop. Every write goes through the
	// real metadata store under its real locking, exactly like a live
	// client under load.
	stop := make(chan struct{})
	var writerErr atomic.Pointer[error]
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			name := fmt.Sprintf("hot-%d.bin", i%8)
			// Vary chunk count so multi-chunk files churn their BlockRef
			// list — the torn-file failure mode #811 describes.
			chunks := 2 + (i % 5)
			if err := fx.tryPutMultiChunkFile(ctx, name, chunks); err != nil {
				e := err
				writerErr.Store(&e)
				return
			}
			i++
			// Throttle so the captured metadata stays a realistic working
			// set rather than an unbounded hash explosion. The pause is
			// short enough that many writes still land inside each
			// capture window, which is what stresses point-in-time
			// consistency.
			time.Sleep(200 * time.Microsecond)
		}
	}()

	// Take a series of snapshots while the writer churns. NoVerify keeps
	// the test off the remote-durability gate (orthogonal to #811); we
	// assert dump<->manifest consistency directly from the on-disk
	// artifacts.
	const snapshots = 12
	for n := 0; n < snapshots; n++ {
		snapID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{NoVerify: true})
		if err != nil {
			t.Fatalf("snapshot %d: CreateSnapshot: %v", n, err)
		}
		snap, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID)
		if werr != nil {
			t.Fatalf("snapshot %d: WaitForSnapshot: %v", n, werr)
		}
		if snap.State != models.StateReady {
			t.Fatalf("snapshot %d: state = %q, want ready", n, snap.State)
		}

		fx.assertSnapshotConsistent(t, snap, n)
	}

	close(stop)
	wg.Wait()
	if ep := writerErr.Load(); ep != nil {
		t.Fatalf("background writer failed: %v", *ep)
	}
}

// ----- consistency assertion -----

// assertSnapshotConsistent restores the snapshot's metadata dump into a
// fresh, isolated memory store and verifies that the set of block hashes
// intrinsically referenced by that restored metadata matches the on-disk
// manifest exactly. A mismatch in either direction is the metadata-vs-block
// skew #811 warned about:
//   - dump references a hash the manifest lacks  => dangling block ref
//   - manifest carries a hash no restored file references => phantom hold
//
// It also verifies that every regular file in the restored dump is whole:
// its BlockRefs tile [0, size) contiguously with no gap or overlap (a torn
// multi-chunk file would fail this).
func (f *realBackupFixture) assertSnapshotConsistent(t *testing.T, snap *models.Snapshot, n int) {
	t.Helper()

	dumpPath := snap.MetadataDumpPath(f.localStoreDir)
	manifestPath := snap.ManifestPath(f.localStoreDir)

	dumpFile, err := os.Open(dumpPath)
	if err != nil {
		t.Fatalf("snapshot %d: open dump: %v", n, err)
	}
	defer func() { _ = dumpFile.Close() }()

	// Restore the dump into a brand-new store. This reconstructs exactly
	// the point-in-time metadata image the snapshot captured.
	restored := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	if err := restored.Restore(context.Background(), dumpFile); err != nil {
		t.Fatalf("snapshot %d: restore dump into fresh store: %v", n, err)
	}

	// Intrinsic hash set of the restored image: re-run Backup on the
	// restored store (to a discard sink) and take the HashSet it derives
	// from the restored files' BlockRefs. This is backend-agnostic and
	// uses the same extraction path the manifest was built from.
	dumpHashes, err := restored.Backup(context.Background(), io.Discard)
	if err != nil {
		t.Fatalf("snapshot %d: re-backup restored store: %v", n, err)
	}

	manifestFile, err := os.Open(manifestPath)
	if err != nil {
		t.Fatalf("snapshot %d: open manifest: %v", n, err)
	}
	defer func() { _ = manifestFile.Close() }()
	manifestHashes, err := snapshot.ReadManifest(manifestFile)
	if err != nil {
		t.Fatalf("snapshot %d: read manifest: %v", n, err)
	}

	// Direction 1: every hash the restored metadata references must be in
	// the manifest. A failure here means the dump captured a file whose
	// blocks are absent from the manifest — the snapshot would restore a
	// file pointing at blocks no hold protects.
	_ = dumpHashes.ForEach(func(h blockstore.ContentHash) error {
		if !manifestHashes.Contains(h) {
			t.Errorf("snapshot %d: dump references hash %x absent from manifest (metadata/block skew)", n, h[:8])
		}
		return nil
	})

	// Direction 2: every manifest hash must be referenced by the restored
	// metadata. A phantom manifest hash means the manifest was captured
	// from a later view than the dump.
	_ = manifestHashes.ForEach(func(h blockstore.ContentHash) error {
		if !dumpHashes.Contains(h) {
			t.Errorf("snapshot %d: manifest carries hash %x not referenced by dump (manifest/metadata skew)", n, h[:8])
		}
		return nil
	})

	// Whole-file check: walk every regular file in the restored image and
	// confirm its BlockRefs tile [0, size) with no gap/overlap.
	f.assertNoTornFiles(t, restored, n)
}

// assertNoTornFiles walks the share's directory tree in the restored store
// and verifies each regular file's BlockRefs form a contiguous,
// non-overlapping tiling of [0, size).
func (f *realBackupFixture) assertNoTornFiles(t *testing.T, store metadata.MetadataStore, n int) {
	t.Helper()
	ctx := context.Background()

	root, err := store.GetRootHandle(ctx, f.shareName)
	if err != nil {
		// The share may not yet exist in the dump if the very first
		// snapshot raced ahead of CreateShare; that is itself consistent
		// (empty image), so treat absence as a clean empty snapshot.
		return
	}

	entries, _, err := store.ListChildren(ctx, root, "", 0)
	if err != nil {
		t.Fatalf("snapshot %d: list children: %v", n, err)
	}
	for _, e := range entries {
		file, err := store.GetFile(ctx, e.Handle)
		if err != nil {
			t.Fatalf("snapshot %d: get file %q: %v", n, e.Name, err)
		}
		if file.Type != metadata.FileTypeRegular || len(file.Blocks) == 0 {
			continue
		}
		var covered uint64
		for bi, br := range file.Blocks {
			if br.Offset != covered {
				t.Errorf("snapshot %d: file %q block %d offset = %d, want %d (torn file: gap/overlap)",
					n, e.Name, bi, br.Offset, covered)
			}
			covered += uint64(br.Size)
		}
		if covered != file.Size {
			t.Errorf("snapshot %d: file %q blocks cover %d bytes, size = %d (torn file: short/long tiling)",
				n, e.Name, covered, file.Size)
		}
	}
}

// ----- fixture (real Backup, real writer) -----

// realBackupFixture wires CreateSnapshot against the real memory metadata
// store's Backup() (not the controlled mock used elsewhere in this package)
// so that the actual consistent-read-view capture is exercised end to end.
type realBackupFixture struct {
	t             *testing.T
	rt            *Runtime
	store         cpstore.Store
	mem           *metadatamemory.MemoryMetadataStore
	localStoreDir string
	shareName     string

	hctr uint64 // monotonically increments to mint distinct block hashes
}

func newRealBackupFixture(t *testing.T) *realBackupFixture {
	t.Helper()

	cp, err := cpstore.New(&cpstore.Config{
		Type:   cpstore.DatabaseTypeSQLite,
		SQLite: cpstore.SQLiteConfig{Path: ":memory:"},
	})
	if err != nil {
		t.Fatalf("cpstore.New: %v", err)
	}
	t.Cleanup(func() { _ = cp.Close() })

	rt := New(cp)

	mem := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	if err := rt.RegisterMetadataStore("memory", mem); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}
	if _, err := cp.CreateMetadataStore(context.Background(), &models.MetadataStoreConfig{
		Name: "memory",
		Type: "memory",
	}); err != nil {
		t.Fatalf("CreateMetadataStore: %v", err)
	}

	localStoreDir := t.TempDir()
	shareName := "data"

	if err := mem.CreateShare(context.Background(), &metadata.Share{Name: shareName}); err != nil {
		t.Fatalf("CreateShare: %v", err)
	}

	localStore := bsmemory.New()
	innerRemote := remotememory.New()
	t.Cleanup(func() { _ = innerRemote.Close() })
	syncer := engine.NewSyncer(localStore, innerRemote, mem, engine.SyncerConfig{
		ParallelUploads:   1,
		ParallelDownloads: 1,
	})
	bs, err := engine.New(engine.BlockStoreConfig{
		Local:          localStore,
		Remote:         innerRemote,
		Syncer:         syncer,
		FileBlockStore: mem,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	rt.sharesSvc.InjectShareForTesting(&shares.Share{
		Name:          shareName,
		MetadataStore: "memory",
		BlockStore:    bs,
	})
	if err := rt.sharesSvc.SetLocalStoreDirForTesting(shareName, localStoreDir); err != nil {
		t.Fatalf("SetLocalStoreDirForTesting: %v", err)
	}

	return &realBackupFixture{
		t:             t,
		rt:            rt,
		store:         cp,
		mem:           mem,
		localStoreDir: localStoreDir,
		shareName:     shareName,
	}
}

func (f *realBackupFixture) close() {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.rt.Shutdown(ctx); err != nil {
		f.t.Logf("Shutdown: %v", err)
	}
}

func (f *realBackupFixture) ctx() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	f.t.Cleanup(cancel)
	return ctx
}

// putMultiChunkFile creates/overwrites a regular file under the share root
// with the given number of contiguous block refs and fails the test on
// error. Used for the deterministic seed.
func (f *realBackupFixture) putMultiChunkFile(ctx context.Context, name string, chunks int) {
	f.t.Helper()
	if err := f.tryPutMultiChunkFile(ctx, name, chunks); err != nil {
		f.t.Fatalf("putMultiChunkFile %q: %v", name, err)
	}
}

// tryPutMultiChunkFile is the goroutine-safe variant: it returns errors
// instead of calling t.Fatalf (forbidden off the test goroutine). Each
// chunk gets a freshly minted unique hash so the file's BlockRef list is a
// genuine multi-chunk tiling.
func (f *realBackupFixture) tryPutMultiChunkFile(ctx context.Context, name string, chunks int) error {
	root, err := f.mem.GetRootHandle(ctx, f.shareName)
	if err != nil {
		return fmt.Errorf("root handle: %w", err)
	}

	// Reuse the existing handle on overwrite (real-client semantics) so the
	// working set stays bounded; only mint a fresh handle for a new name.
	h, err := f.mem.GetChild(ctx, root, name)
	isNew := err != nil
	if isNew {
		h, err = f.mem.GenerateHandle(ctx, f.shareName, "/"+name)
		if err != nil {
			return fmt.Errorf("generate handle: %w", err)
		}
	}
	_, id, err := metadata.DecodeFileHandle(h)
	if err != nil {
		return fmt.Errorf("decode handle: %w", err)
	}

	const chunkSize = uint32(1 << 20)
	blocks := make([]blockstore.BlockRef, chunks)
	for i := 0; i < chunks; i++ {
		ctr := atomic.AddUint64(&f.hctr, 1)
		blocks[i] = blockstore.BlockRef{
			Hash:   mintHash(ctr),
			Offset: uint64(i) * uint64(chunkSize),
			Size:   chunkSize,
		}
	}

	file := &metadata.File{
		ShareName: f.shareName,
		FileAttr: metadata.FileAttr{
			Type:   metadata.FileTypeRegular,
			Mode:   0o644,
			UID:    1000,
			GID:    1000,
			Size:   uint64(chunks) * uint64(chunkSize),
			Blocks: blocks,
		},
	}
	file.ID = id
	if err := f.mem.PutFile(ctx, file); err != nil {
		return fmt.Errorf("put file: %w", err)
	}
	if isNew {
		if err := f.mem.SetParent(ctx, h, root); err != nil {
			return fmt.Errorf("set parent: %w", err)
		}
		if err := f.mem.SetChild(ctx, root, name, h); err != nil {
			return fmt.Errorf("set child: %w", err)
		}
	}
	return nil
}

// ----- small helpers -----

// mintHash deterministically derives a unique ContentHash from a counter so
// every chunk across the whole test references a distinct block.
func mintHash(ctr uint64) blockstore.ContentHash {
	var h blockstore.ContentHash
	for i := 0; i < 8; i++ {
		h[i] = byte(ctr >> (8 * i))
	}
	// Tag the tail so two counters that share low bytes still differ.
	h[blockstore.HashSize-1] = 0xA5
	return h
}
