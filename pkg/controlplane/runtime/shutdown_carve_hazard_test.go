package runtime

import (
	"context"
	"crypto/rand"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	adaptercommon "github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	cpstore "github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/metadata"
	metamem "github.com/marmos91/dittofs/pkg/metadata/store/memory"
	sqlitemeta "github.com/marmos91/dittofs/pkg/metadata/store/sqlite"
)

// gatedMetaStore observes when the metadata store is closed and can park one
// transaction across that close, so a carve commit is provably in flight while
// the shutdown sequence runs.
type gatedMetaStore struct {
	metadata.Store

	mu         sync.Mutex
	closed     bool
	armed      bool
	afterClose int

	enteredOnce sync.Once
	entered     chan struct{}
	release     chan struct{}
}

func newGatedMetaStore(s metadata.Store) *gatedMetaStore {
	return &gatedMetaStore{
		Store:   s,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (g *gatedMetaStore) arm() {
	g.mu.Lock()
	g.armed = true
	g.mu.Unlock()
}

func (g *gatedMetaStore) Close() error {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
	if c, ok := g.Store.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

func (g *gatedMetaStore) commitsAfterClose() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.afterClose
}

func (g *gatedMetaStore) WithTransaction(ctx context.Context, fn func(metadata.Transaction) error) error {
	g.mu.Lock()
	park := g.armed
	g.armed = false
	g.mu.Unlock()

	if park {
		g.enteredOnce.Do(func() { close(g.entered) })
		<-g.release
	}

	g.mu.Lock()
	if g.closed {
		g.afterClose++
	}
	g.mu.Unlock()

	return g.Store.WithTransaction(ctx, fn)
}

// TestShutdownLeavesCarveCommittingThroughAClosedMetadataStore runs the shutdown
// steps the server actually takes — fence the rollups, then close the metadata
// stores — with a share's carve pass in flight, and reports whether a manifest
// commit still reaches the metadata store afterwards.
func TestShutdownLeavesCarveCommittingThroughAClosedMetadataStore(t *testing.T) {
	ctx := context.Background()

	cps, err := cpstore.New(&cpstore.Config{
		Type:   cpstore.DatabaseTypeSQLite,
		SQLite: cpstore.SQLiteConfig{Path: ":memory:"},
	})
	if err != nil {
		t.Fatalf("cpstore.New: %v", err)
	}
	t.Cleanup(func() { _ = cps.Close() })

	bsID, err := cps.CreateBlockStore(ctx, &models.BlockStoreConfig{Name: "blocks", Type: "memory"})
	if err != nil {
		t.Fatalf("CreateBlockStore: %v", err)
	}

	gated := newGatedMetaStore(metamem.NewMemoryMetadataStoreWithDefaults())

	rt := New(cps)
	rt.SetLocalStoreDefaults(&shares.LocalStoreDefaults{JournalRoot: t.TempDir()})
	if err := rt.RegisterMetadataStore("meta", gated); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}

	const shareName = "/carve-shutdown"
	if err := rt.AddShare(ctx, &ShareConfig{
		Name:          shareName,
		MetadataStore: "meta",
		BlockStoreID:  bsID,
		Enabled:       true,
	}); err != nil {
		t.Fatalf("AddShare: %v", err)
	}

	bs, err := rt.GetBlockStoreForShare(shareName)
	if err != nil {
		t.Fatalf("GetBlockStoreForShare: %v", err)
	}

	payload := make([]byte, 4<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("rand: %v", err)
	}

	// Arm before the write so the carve dispatcher's first manifest commit is
	// the transaction that parks.
	gated.arm()
	if err := adaptercommon.WriteToBlockStore(ctx, bs, metadata.PayloadID("p1"), payload, 0); err != nil {
		t.Fatalf("WriteToBlockStore: %v", err)
	}

	select {
	case <-gated.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("no metadata transaction parked: the carve dispatcher never committed, so the premise is untested")
	}

	// The shutdown sequence the server takes: lifecycle.Service.shutdown calls
	// StopRollups and then CloseMetadataStores, and nothing in between closes
	// the share's block store.
	stopCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rt.StopRollups(stopCtx)
	rt.CloseMetadataStores()

	close(gated.release)

	// Give the parked commit and any successors time to land.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && gated.commitsAfterClose() == 0 {
		time.Sleep(50 * time.Millisecond)
	}

	t.Logf("metadata transactions that ran AFTER CloseMetadataStores: %d", gated.commitsAfterClose())
	if n := gated.commitsAfterClose(); n > 0 {
		t.Errorf("HAZARD LIVE: %d carve commits reached the metadata store after it was closed", n)
	}

	_ = bs.Close()
}

// errRecordingStore records the outcome of every transaction that runs after
// the metadata store has been closed.
type errRecordingStore struct {
	metadata.Store

	mu        sync.Mutex
	closed    bool
	afterErrs []error
}

func (e *errRecordingStore) Close() error {
	e.mu.Lock()
	e.closed = true
	e.mu.Unlock()
	if c, ok := e.Store.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

func (e *errRecordingStore) WithTransaction(ctx context.Context, fn func(metadata.Transaction) error) error {
	err := e.Store.WithTransaction(ctx, fn)
	e.mu.Lock()
	if e.closed {
		e.afterErrs = append(e.afterErrs, err)
	}
	e.mu.Unlock()
	return err
}

func (e *errRecordingStore) snapshot() []error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]error(nil), e.afterErrs...)
}

// TestShutdownCarveHitsAClosedSQLiteMetadataStore is the same sequence against a
// real SQL-backed metadata store and with no transaction artificially parked:
// the shutdown steps run before the carve dispatcher's first tick, and the
// commit that follows lands on a database the shutdown already closed.
//
// The "quiesced" subtest adds the one step the sequence is missing — closing
// the share's block stores, which stops and drains each syncer — and shows the
// same run reaching the metadata store zero times afterwards.
func TestShutdownCarveHitsAClosedSQLiteMetadataStore(t *testing.T) {
	t.Run("server sequence", func(t *testing.T) {
		errs := runShutdownSequence(t, nil)
		t.Logf("transactions that ran after CloseMetadataStores: %d", len(errs))
		for i, e := range errs {
			t.Logf("  [%d] %v", i, e)
		}
		if len(errs) > 0 {
			t.Errorf("HAZARD LIVE: %d transactions reached the metadata store after shutdown closed it", len(errs))
		}
	})

	t.Run("quiesced", func(t *testing.T) {
		errs := runShutdownSequence(t, func(rt *Runtime) { rt.sharesSvc.CloseBlockStores() })
		t.Logf("transactions that ran after CloseMetadataStores: %d", len(errs))
		for i, e := range errs {
			t.Logf("  [%d] %v", i, e)
		}
		if len(errs) > 0 {
			t.Errorf("closing the block stores first did not fence the carve: %d transactions still landed", len(errs))
		}
	})
}

// runShutdownSequence builds a remote-backed share over a real SQLite metadata
// store, writes a payload the carve dispatcher will pick up, then runs the
// server's shutdown steps with quiesce (if any) inserted between StopRollups
// and CloseMetadataStores. It returns the outcome of every transaction that
// reached the metadata store after it was closed.
func runShutdownSequence(t *testing.T, quiesce func(*Runtime)) []error {
	t.Helper()
	ctx := context.Background()

	cps, err := cpstore.New(&cpstore.Config{
		Type:   cpstore.DatabaseTypeSQLite,
		SQLite: cpstore.SQLiteConfig{Path: ":memory:"},
	})
	if err != nil {
		t.Fatalf("cpstore.New: %v", err)
	}
	t.Cleanup(func() { _ = cps.Close() })

	bsID, err := cps.CreateBlockStore(ctx, &models.BlockStoreConfig{Name: "blocks", Type: "memory"})
	if err != nil {
		t.Fatalf("CreateBlockStore: %v", err)
	}

	sq, err := sqlitemeta.NewSQLiteMetadataStore(ctx, &sqlitemeta.SQLiteMetadataStoreConfig{
		Path:        filepath.Join(t.TempDir(), "meta.db"),
		AutoMigrate: true,
	}, metadata.FilesystemCapabilities{})
	if err != nil {
		t.Fatalf("NewSQLiteMetadataStore: %v", err)
	}
	rec := &errRecordingStore{Store: sq}

	rt := New(cps)
	rt.SetLocalStoreDefaults(&shares.LocalStoreDefaults{JournalRoot: t.TempDir()})
	if err := rt.RegisterMetadataStore("meta", rec); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}

	const shareName = "/carve-shutdown-sql"
	if err := rt.AddShare(ctx, &ShareConfig{
		Name:          shareName,
		MetadataStore: "meta",
		BlockStoreID:  bsID,
		Enabled:       true,
	}); err != nil {
		t.Fatalf("AddShare: %v", err)
	}

	bs, err := rt.GetBlockStoreForShare(shareName)
	if err != nil {
		t.Fatalf("GetBlockStoreForShare: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })

	payload := make([]byte, 4<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("rand: %v", err)
	}
	if err := adaptercommon.WriteToBlockStore(ctx, bs, metadata.PayloadID("p1"), payload, 0); err != nil {
		t.Fatalf("WriteToBlockStore: %v", err)
	}

	stopCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rt.StopRollups(stopCtx)
	if quiesce != nil {
		quiesce(rt)
	}
	rt.CloseMetadataStores()

	// The carve dispatcher ticks on its own interval; StopRollups does not
	// stop it.
	time.Sleep(8 * time.Second)

	return rec.snapshot()
}
