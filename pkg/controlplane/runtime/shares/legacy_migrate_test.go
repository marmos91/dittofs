package shares

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/local/fs"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	metamem "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newMigrateEngine builds a full engine.Store over an fs local store rooted at
// dir + a memory remote + memory metadata, mirroring the engine drain-reset
// fixture. ManualSync keeps DrainRollups the deterministic carve driver so the
// remote block sink and FileChunk manifest are populated on demand.
func newMigrateEngine(t *testing.T, dir string, ms *metamem.MemoryMetadataStore, rem *remotememory.Store, migrate bool) *engine.Store {
	t.Helper()
	localStore, err := fs.NewWithOptions(dir, 100*1024*1024, ms, fs.FSStoreOptions{MigrateLegacyLayout: migrate})
	if err != nil {
		t.Fatalf("fs.NewWithOptions: %v", err)
	}
	cfg := engine.DefaultConfig()
	cfg.ManualSync = true
	// Wrap the shared memory remote so an engine Close() does not close it — the
	// remote outlives the first store the same way ref-counting keeps it alive in
	// production (a second engine reopens over the same remote after the upgrade).
	engineRemote := &nonClosingRemote{rem}
	syncer := engine.NewSyncer(localStore, engineRemote, ms, cfg)
	bs, err := engine.New(engine.BlockStoreConfig{
		Local:           localStore,
		Remote:          engineRemote,
		Syncer:          syncer,
		FileChunkStore:  ms,
		SyncedHashStore: ms,
		ReadBufferBytes: 64 * 1024 * 1024,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	syncer.SetRemoteBlockStore(rem)
	if err := bs.Start(context.Background()); err != nil {
		t.Fatalf("engine.Start: %v", err)
	}
	// The fs store's migration flag is consumed here, the same way
	// ConfigureBlockStore does it after bs.Start.
	if m, ok := any(localStore).(interface{ MigratedFromLegacy() bool }); ok && m.MigratedFromLegacy() {
		if err := SeedColdFromManifest(context.Background(), bs, ms); err != nil {
			t.Fatalf("SeedColdFromManifest: %v", err)
		}
	}
	return bs
}

// legacyShareFiles is the content a pre-journal share was holding before the
// upgrade. Sizes straddle the sub-chunk and multi-chunk read paths.
var legacyShareFiles = map[string][]byte{
	"small-a":  bytes.Repeat([]byte{0x11}, 4096),
	"small-b":  []byte("the quick brown fox jumps over the lazy dog"),
	"multi-mb": bytes.Repeat([]byte{0x5A, 0xA5}, 3*1024*1024),
}

// plantPreJournalShare writes legacyShareFiles through a pre-upgrade engine (so
// the bytes land in the remote and the FileChunk manifest), then replaces the
// local journal with the leftover blobs/+logs/ of a pre-journal release. The
// returned remote and metadata store survive the upgrade intact, exactly as they
// do in the field — only the local tier is legacy.
func plantPreJournalShare(t *testing.T) (string, *metamem.MemoryMetadataStore, *remotememory.Store) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	ms := metamem.NewMemoryMetadataStoreWithDefaults()
	t.Cleanup(func() { _ = ms.Close() })
	rem := remotememory.New()

	bs1 := newMigrateEngine(t, dir, ms, rem, false)
	for id, data := range legacyShareFiles {
		if _, err := bs1.WriteAt(ctx, id, nil, data, 0); err != nil {
			t.Fatalf("WriteAt %s: %v", id, err)
		}
	}
	// Carve to remote + populate the FileChunk manifest, then close so the local
	// journal is quiescent.
	if err := bs1.DrainRollups(ctx); err != nil {
		t.Fatalf("DrainRollups: %v", err)
	}
	if err := bs1.Close(); err != nil {
		t.Fatalf("close bs1: %v", err)
	}

	if err := os.RemoveAll(filepath.Join(dir, "journal")); err != nil {
		t.Fatalf("wipe journal: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "blobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blobs", "0000000000000000.blob"), []byte("legacy"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "logs", "share"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "logs", "share", "f.log"), []byte("legacy"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, ms, rem
}

// readAllLegacyFiles asserts every planted file reads back byte-identical.
func readAllLegacyFiles(t *testing.T, bs *engine.Store, when string) {
	t.Helper()
	ctx := context.Background()
	for id, want := range legacyShareFiles {
		got := make([]byte, len(want))
		n, err := bs.ReadAt(ctx, id, nil, got, 0)
		if err != nil {
			t.Fatalf("ReadAt %s %s: %v", id, when, err)
		}
		if n != len(want) || !bytes.Equal(got, want) {
			t.Fatalf("ReadAt %s %s: read %d bytes, byte-identical=%v; served wrong bytes",
				id, when, n, bytes.Equal(got, want))
		}
	}
}

// TestLegacyRemoteMigration_ReadsColdFromRemote is the whole point of the
// remote-backed upgrade path: a share whose local dir carried the pre-journal
// blobs/+logs/ layout must, after migration, serve byte-identical data by cold-
// fetching from the remote via the surviving metadata manifest — not zeros.
func TestLegacyRemoteMigration_ReadsColdFromRemote(t *testing.T) {
	dir, ms, rem := plantPreJournalShare(t)

	bs2 := newMigrateEngine(t, dir, ms, rem, true)
	t.Cleanup(func() { _ = bs2.Close() })

	// Legacy dirs archived aside, not destroyed.
	for _, sub := range []string{"blobs", "logs"} {
		if _, err := os.Stat(filepath.Join(dir, sub+".pre-journal-backup")); err != nil {
			t.Fatalf("legacy %s not archived: %v", sub, err)
		}
	}
	readAllLegacyFiles(t, bs2, "after migration")
}

// TestLegacyRemoteMigration_ReadsColdAfterRestart is the field failure: the
// migration completes, the server restarts, and only then is a file read. The
// cold intervals the migration seeded are what make that read fetch instead of
// zero-fill, so they must be durable — a restart that loses them turns every
// range back into a POSIX hole and the share silently serves zeros of the right
// length with no remote fetch at all. Nothing re-seeds on the second start: the
// legacy dirs are already archived, so the migration does not run again.
//
// No read happens before the restart, deliberately: a read would hydrate the
// bytes into the journal as warm records and mask the lost markers.
func TestLegacyRemoteMigration_ReadsColdAfterRestart(t *testing.T) {
	dir, ms, rem := plantPreJournalShare(t)

	bs2 := newMigrateEngine(t, dir, ms, rem, true)
	if err := bs2.Close(); err != nil {
		t.Fatalf("close bs2: %v", err)
	}

	bs3 := newMigrateEngine(t, dir, ms, rem, false)
	t.Cleanup(func() { _ = bs3.Close() })
	readAllLegacyFiles(t, bs3, "after migration + restart")
}
