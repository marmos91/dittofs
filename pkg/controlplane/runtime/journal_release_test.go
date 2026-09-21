package runtime

import (
	"context"
	"os"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journal"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	cpstore "github.com/marmos91/dittofs/pkg/controlplane/store"
	metamem "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// A share holds its journal's append log and index open for as long as it is
// registered, so shutting the runtime down has to release them: a platform
// that refuses to unlink an open file cannot remove the journal root while one
// is still held, and the directory outlives the process that opened it.
//
// The shutdown under test is the server's own — Serve, then cancel — because
// that is the only one production runs. Assembling the steps here instead would
// assert about a sequence no process takes.
func TestShutdownReleasesShareJournals(t *testing.T) {
	ctx := context.Background()

	cps, err := cpstore.New(&cpstore.Config{
		Type:   cpstore.DatabaseTypeSQLite,
		SQLite: cpstore.SQLiteConfig{Path: ":memory:"},
	})
	if err != nil {
		t.Fatalf("cpstore.New: %v", err)
	}
	t.Cleanup(func() { _ = cps.Close() })

	bsID, err := cps.CreateBlockStore(ctx, &models.BlockStoreConfig{
		Name: "test-blocks", Type: "memory",
	})
	if err != nil {
		t.Fatalf("CreateBlockStore: %v", err)
	}

	// Not a t.TempDir(): the test removes the root itself, while the runtime is
	// still reachable, so the removal is the assertion rather than a teardown
	// step whose failure is only reported as a cleanup warning.
	journalRoot, err := os.MkdirTemp("", "journal-release")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(journalRoot) })

	rt := New(cps)
	rt.SetLocalStoreDefaults(&shares.LocalStoreDefaults{JournalRoot: journalRoot})
	rt.SetSnapshotSchedulerConfig(0, true)
	if err := rt.RegisterMetadataStore("test-meta", metamem.NewMemoryMetadataStoreWithDefaults()); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}

	const shareName = "/journal-release"
	if err := rt.AddShare(ctx, &ShareConfig{
		Name:          shareName,
		MetadataStore: "test-meta",
		BlockStoreID:  bsID,
		Enabled:       true,
	}); err != nil {
		t.Fatalf("AddShare: %v", err)
	}

	bs, err := rt.GetBlockStoreForShare(shareName)
	if err != nil {
		t.Fatalf("GetBlockStoreForShare: %v", err)
	}
	jrn, ok := bs.Local().(*journal.Store)
	if !ok {
		t.Fatalf("the share's local tier is %T, want a journal store", bs.Local())
	}
	// Without this the assertion after shutdown could pass on a journal that
	// was never open in the first place.
	if jrn.Closed() {
		t.Fatal("the share's journal is closed while the share is still registered")
	}

	serveUntilShutdown(t, rt)

	if !jrn.Closed() {
		t.Error("the share's journal is still open after shutdown")
	}
	if err := os.RemoveAll(journalRoot); err != nil {
		t.Errorf("journal root not removable after shutdown: %v", err)
	}
}
