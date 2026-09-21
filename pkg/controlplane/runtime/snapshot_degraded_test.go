package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	badgerdb "github.com/dgraph-io/badger/v4"
	"github.com/marmos91/dittofs/pkg/block/engine"
	bsmemory "github.com/marmos91/dittofs/pkg/block/local/memory"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	cpstore "github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatabadger "github.com/marmos91/dittofs/pkg/metadata/store/badger"
	"github.com/marmos91/dittofs/pkg/snapshot"
)

// degradedFixture is a share whose metadata store holds one inode row that no
// codec can decode — the state an operator is trying to get away from when
// they reach for restore.
type degradedFixture struct {
	rt            *Runtime
	meta          *metadatabadger.BadgerMetadataStore
	shareName     string
	localStoreDir string
	badKey        string
}

// newDegradedFixture seeds a badger store with a root and one file, clobbers
// the file's f: row on disk (the store must be closed for that: badger holds a
// directory lock), then reopens it behind a disabled, remote-less share.
func newDegradedFixture(t *testing.T) *degradedFixture {
	t.Helper()
	ctx := context.Background()
	dbDir := filepath.Join(t.TempDir(), "db")

	seed, err := metadatabadger.NewBadgerMetadataStoreWithDefaults(ctx, dbDir)
	if err != nil {
		t.Fatalf("NewBadgerMetadataStoreWithDefaults: %v", err)
	}
	const shareName = "degraded-share"
	if _, err := seed.CreateRootDirectory(ctx, shareName, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0o755,
	}); err != nil {
		t.Fatalf("CreateRootDirectory: %v", err)
	}
	h, err := seed.GenerateHandle(ctx, shareName, "/doomed.bin")
	if err != nil {
		t.Fatalf("GenerateHandle: %v", err)
	}
	_, fileID, err := metadata.DecodeFileHandle(h)
	if err != nil {
		t.Fatalf("DecodeFileHandle: %v", err)
	}
	if err := seed.UpdateAttrs(ctx, &metadata.File{
		ID: fileID, ShareName: shareName, Path: "/doomed.bin",
		FileAttr: metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o644},
	}); err != nil {
		t.Fatalf("UpdateAttrs: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("Close seed store: %v", err)
	}

	badKey := clobberOneInodeRow(t, dbDir, fileID.String())

	meta, err := metadatabadger.NewBadgerMetadataStoreWithDefaults(ctx, dbDir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = meta.Close() })

	cp, err := cpstore.New(&cpstore.Config{
		Type:   cpstore.DatabaseTypeSQLite,
		SQLite: cpstore.SQLiteConfig{Path: ":memory:"},
	})
	if err != nil {
		t.Fatalf("cpstore.New: %v", err)
	}
	t.Cleanup(func() { _ = cp.Close() })

	rt := New(cp)
	setJournalRoot(t, rt)

	const metaStoreName = "badger-degraded"
	if err := rt.RegisterMetadataStore(metaStoreName, meta); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}
	if _, err := cp.CreateMetadataStore(ctx, &models.MetadataStoreConfig{
		Name: metaStoreName,
		Type: "badger",
	}); err != nil {
		t.Fatalf("CreateMetadataStore: %v", err)
	}

	// No remote: the restore path then skips the HEAD-probe verify, keeping the
	// test about the safety snapshot rather than about durability plumbing.
	localStore := bsmemory.New()
	syncer := engine.NewRemoteSync(localStore, nil, meta, engine.RemoteSyncConfig{ParallelDownloads: 1})
	bs, err := engine.New(engine.BlockStoreConfig{
		Local:          localStore,
		RemoteSync:     syncer,
		FileChunkStore: meta,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	localStoreDir := t.TempDir()
	rt.sharesSvc.InjectShareForTesting(&shares.Share{
		Name:          shareName,
		MetadataStore: metaStoreName,
		BlockStore:    bs,
		Enabled:       false, // restore requires the share disabled
	})
	if err := rt.sharesSvc.SetLocalStoreDirForTesting(shareName, localStoreDir); err != nil {
		t.Fatalf("SetLocalStoreDirForTesting: %v", err)
	}
	t.Cleanup(func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = rt.Shutdown(shutCtx)
	})

	return &degradedFixture{
		rt: rt, meta: meta, shareName: shareName,
		localStoreDir: localStoreDir, badKey: badKey,
	}
}

// clobberOneInodeRow makes the f: row for fileID undecodable and returns its
// key. Leaving every other row intact is what keeps the share usable and the
// gap a single, nameable entry.
func clobberOneInodeRow(t *testing.T, dir, fileID string) string {
	t.Helper()
	db, err := badgerdb.Open(badgerdb.DefaultOptions(dir).WithLogger(nil))
	if err != nil {
		t.Fatalf("reopen badger raw: %v", err)
	}
	defer func() { _ = db.Close() }()

	key := "f:" + fileID
	if err := db.Update(func(txn *badgerdb.Txn) error {
		if _, gerr := txn.Get([]byte(key)); gerr != nil {
			return gerr
		}
		return txn.Set([]byte(key), []byte("undecodable"))
	}); err != nil {
		t.Fatalf("clobber %s: %v", key, err)
	}
	return key
}

// TestSnapshot_DefaultRefusesDegradedStore pins requirement one half: an
// ordinary snapshot of a store holding an undecodable row still fails. Nobody
// gets a short snapshot by asking for a snapshot.
func TestSnapshot_DefaultRefusesDegradedStore(t *testing.T) {
	fx := newDegradedFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	snapID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{NoVerify: true})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	snap, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID)
	if werr == nil && snap.State == models.StateReady {
		t.Fatalf("snapshot %s reached state=ready over a store with an undecodable row; "+
			"the default path must refuse rather than ship a short manifest", snapID)
	}
	if snap != nil && snap.Degraded {
		t.Fatalf("a refused snapshot must not be recorded as degraded; degraded snapshots are the opt-in path")
	}
}

// TestSnapshot_DegradedIsLabelledAndRestoreStaysReachable is the regression
// that matters. Refusing to snapshot a store with an undecodable row makes the
// share un-snapshottable AND un-restorable, because restore takes a safety
// snapshot before it resets the store — so the refusal blocks recovery from
// exactly the corruption that caused it, and leaves the operator with no undo
// point at all.
//
// The rule being enforced is not "never snapshot a damaged store", it is
// "never let a snapshot misrepresent its own completeness". So the safety snap
// completes, and it is labelled: on its row, and in its own directory, where
// the marker travels with the snapshot.
func TestSnapshot_DegradedIsLabelledAndRestoreStaysReachable(t *testing.T) {
	fx := newDegradedFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// A source snapshot to restore from. Only the degraded path can produce one
	// over this store, which is itself the point.
	srcID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{
		NoVerify:      true,
		AllowDegraded: true,
	})
	if err != nil {
		t.Fatalf("CreateSnapshot(source): %v", err)
	}
	src, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, srcID)
	if werr != nil {
		t.Fatalf("WaitForSnapshot(source): %v", werr)
	}
	if src.State != models.StateReady {
		t.Fatalf("source snapshot state = %q (%s), want ready", src.State, src.Error)
	}
	assertLabelledDegraded(t, fx, src, 1)

	// The restore itself. Pre-fix this stops at the safety snapshot with
	// ErrRestoreSafetySnapFailed and no undo point is ever created.
	//
	// What is asserted is the undo point, not that this particular restore
	// succeeds end to end: restoring a degraded snapshot back over its own
	// store puts the undecodable row back, so the post-restore hash
	// enumeration legitimately refuses on it. That refusal is the GC mark
	// pass's fail-closed walk working, and it names the row. The operator's
	// actual move — restoring a GOOD snapshot over a damaged store — does not
	// hit it. What was broken, and is fixed here, is that recovery could not
	// even begin.
	safetyID, rerr := fx.rt.RestoreSnapshot(ctx, fx.shareName, srcID, RestoreSnapshotOpts{
		AllowNonDurable: true,
	})
	if errors.Is(rerr, models.ErrRestoreSafetySnapFailed) {
		t.Fatalf("restore refused at the safety snapshot: %v; "+
			"a store with an undecodable row is exactly what an operator restores away from, "+
			"so refusing here blocks recovery from the condition it detected and leaves no undo point", rerr)
	}
	if safetyID == "" {
		t.Fatal("restore returned no safety snapshot id; the undo point must exist")
	}

	safety, gerr := fx.rt.GetSnapshot(ctx, fx.shareName, safetyID)
	if gerr != nil {
		t.Fatalf("GetSnapshot(safety): %v", gerr)
	}
	if safety.State != models.StateReady {
		t.Fatalf("safety snapshot state = %q (%s), want ready", safety.State, safety.Error)
	}
	assertLabelledDegraded(t, fx, safety, 1)
}

// assertLabelledDegraded checks both places a degraded snapshot declares
// itself: the control-plane row an operator lists, and the marker inside the
// snapshot directory, which travels with the snapshot to wherever it is read.
func assertLabelledDegraded(t *testing.T, fx *degradedFixture, snap *models.Snapshot, wantEntries int64) {
	t.Helper()
	if !snap.Degraded {
		t.Errorf("snapshot %s: Degraded = false, want true; a snapshot short by %d rows must not read as complete",
			snap.ID, wantEntries)
	}
	if snap.DegradedEntries != wantEntries {
		t.Errorf("snapshot %s: DegradedEntries = %d, want %d", snap.ID, snap.DegradedEntries, wantEntries)
	}

	marker, err := snapshot.ReadDegradedMarker(snap.DegradedMarkerPath(fx.localStoreDir))
	if err != nil {
		t.Fatalf("snapshot %s: ReadDegradedMarker: %v", snap.ID, err)
	}
	if marker == nil {
		t.Fatalf("snapshot %s: no degraded.json in the snapshot directory; "+
			"absence of the marker means complete, so a degraded snapshot without one is indistinguishable from a good one",
			snap.ID)
	}
	if marker.Entries != int(wantEntries) {
		t.Errorf("snapshot %s: marker Entries = %d, want %d", snap.ID, marker.Entries, wantEntries)
	}
	found := false
	for _, k := range marker.Keys {
		if k == fx.badKey {
			found = true
		}
	}
	if !found {
		t.Errorf("snapshot %s: marker Keys = %v, want it to name %q — the keys are what makes the gap findable",
			snap.ID, marker.Keys, fx.badKey)
	}
}
