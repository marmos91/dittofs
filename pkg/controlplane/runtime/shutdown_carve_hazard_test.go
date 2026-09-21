package runtime

import (
	"context"
	"crypto/rand"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	adaptercommon "github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	cpstore "github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/metadata"
	sqlitemeta "github.com/marmos91/dittofs/pkg/metadata/store/sqlite"
)

// closeWatchingStore records the outcome of every transaction that reaches the
// metadata store after it has been closed.
type closeWatchingStore struct {
	metadata.Store

	mu        sync.Mutex
	closed    bool
	afterErrs []error
}

func (c *closeWatchingStore) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return c.Store.Close()
}

func (c *closeWatchingStore) WithTransaction(ctx context.Context, fn func(metadata.Transaction) error) error {
	err := c.Store.WithTransaction(ctx, fn)

	c.mu.Lock()
	if c.closed {
		c.afterErrs = append(c.afterErrs, err)
	}
	c.mu.Unlock()
	return err
}

func (c *closeWatchingStore) wasClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *closeWatchingStore) afterClose() []error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]error(nil), c.afterErrs...)
}

// carveHazardFixture is a runtime with one remote-backed share whose metadata
// store reports any transaction that outlives its own close.
type carveHazardFixture struct {
	rt    *Runtime
	meta  *closeWatchingStore
	share string
}

// newCarveHazardFixture builds a runtime with one remote-backed share over a
// real SQLite metadata store, the backend whose close makes a late commit fail
// rather than quietly succeed.
func newCarveHazardFixture(t *testing.T) *carveHazardFixture {
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
	meta := &closeWatchingStore{Store: sq}

	rt := New(cps)
	rt.SetLocalStoreDefaults(&shares.LocalStoreDefaults{JournalRoot: t.TempDir()})
	rt.SetSnapshotSchedulerConfig(0, true)
	if err := rt.RegisterMetadataStore("meta", meta); err != nil {
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

	return &carveHazardFixture{rt: rt, meta: meta, share: shareName}
}

// write leaves a payload in the share's journal that the carve dispatcher will
// pack into a remote block and commit manifest rows for.
func (f *carveHazardFixture) write(t *testing.T) {
	t.Helper()
	bs, err := f.rt.GetBlockStoreForShare(f.share)
	if err != nil {
		t.Fatalf("GetBlockStoreForShare: %v", err)
	}
	payload := make([]byte, 4<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("rand: %v", err)
	}
	if err := adaptercommon.WriteToBlockStore(context.Background(), bs, metadata.PayloadID("p1"), payload, 0); err != nil {
		t.Fatalf("WriteToBlockStore: %v", err)
	}
}

// serveUntilShutdown runs the server's own lifecycle and then cancels it, so
// the shutdown sequence under test is the one production takes — no step of it
// is supplied by the test.
//
// The context must stay live until startup completes. Serve returns a startup
// error rather than running the shutdown sequence if the cancellation lands
// while a startup step is still using that context, and the assertions below
// then report a shutdown that never ran instead of the startup that failed —
// so the barrier is StartupDone, which closes only once every step that can
// fail is past. Serve's own return is watched alongside it: a failed startup
// leaves StartupDone open, and the error names the cause.
func (f *carveHazardFixture) serveUntilShutdown(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- f.rt.Serve(ctx) }()

	select {
	case <-f.rt.StartupDone():
	case err := <-done:
		t.Fatalf("Serve returned before finishing startup: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("Serve did not finish startup")
	}
	cancel()

	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("Serve did not return after cancellation")
	}
}

// TestServerShutdownQuiescesCarveBeforeClosingMetadataStores is the regression
// guard: the server's shutdown sequence must stop and drain every share's carve
// dispatcher before it closes the metadata stores those commits run through.
//
// The dispatcher ticks on its own interval and runs on a background context, so
// cancelling the runtime's does not reach it. Only closing the share's block
// store stops it. When that step is missing, every tick after the close fails
// with "sql: database is closed" and the chunks it was carving stay local and
// unmirrored — see TestCarveCommitsReachAClosedMetadataStore_WithoutTheFence
// for the same run with the step removed.
func TestServerShutdownQuiescesCarveBeforeClosingMetadataStores(t *testing.T) {
	// Counted against a baseline taken before the fixture exists, so a
	// dispatcher some other test left behind neither satisfies the premise
	// below nor fails the assertion at the end.
	base := carveDispatchers()

	f := newCarveHazardFixture(t)
	f.write(t)

	// A dispatcher that is not running before shutdown makes every assertion
	// below vacuous.
	if carveDispatchers() <= base {
		t.Fatal("the share started no carve dispatcher: there is nothing for the fence to stop")
	}

	f.serveUntilShutdown(t)

	// Long enough for several dispatcher intervals: one still running would
	// commit more than once in this window.
	time.Sleep(5 * time.Second)

	// Without this the count below passes on a run whose metadata store was
	// never closed at all.
	if !f.meta.wasClosed() {
		t.Fatal("shutdown never closed the metadata store: the sequence under test did not run")
	}

	if errs := f.meta.afterClose(); len(errs) > 0 {
		for i, e := range errs {
			t.Logf("  [%d] %v", i, e)
		}
		t.Errorf("%d transactions reached the metadata store after shutdown closed it", len(errs))
	}

	// Stopping the dispatcher is not the same as joining it: one that was told
	// to stop but never waited for can still be inside a commit when the store
	// closes.
	if n := carveDispatchers(); n > base {
		t.Errorf("%d carve dispatcher goroutines are still running after shutdown, %d before the fixture:\n%s",
			n, base, goroutineDump())
	}
}

// carveDispatchers counts the live carve dispatcher goroutines. Naming the
// goroutine rather than counting all of them keeps the answer about the
// data-plane loop and not about whatever else the process happens to be
// running.
func carveDispatchers() int {
	return strings.Count(goroutineDump(), "engine.(*RemoteSync).carveDispatcher")
}

// goroutineDump returns every goroutine's stack. The buffer grows until the
// dump fits: runtime.Stack truncates rather than reporting how much it needed,
// and a truncated dump silently drops the frames the assertions look for.
func goroutineDump() string {
	for size := 1 << 20; ; size *= 2 {
		buf := make([]byte, size)
		if n := runtime.Stack(buf, true); n < size {
			return string(buf[:n])
		}
	}
}

// TestCarveCommitsReachAClosedMetadataStore_WithoutTheFence is the
// counterfactual for the guard above. It runs the same fixture but closes the
// metadata stores directly, skipping the block-store close, and requires the
// carve dispatcher to keep committing into the closed store. Without this the
// guard could pass on a fixture whose dispatcher never ran at all.
func TestCarveCommitsReachAClosedMetadataStore_WithoutTheFence(t *testing.T) {
	f := newCarveHazardFixture(t)
	f.write(t)
	t.Cleanup(func() { f.rt.sharesSvc.CloseBlockStores(context.Background()) })

	f.rt.CloseMetadataStores()

	// Returns as soon as the dispatcher commits, so this costs one interval
	// rather than the whole window.
	deadline := time.Now().Add(30 * time.Second)
	var errs []error
	for len(errs) == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		errs = f.meta.afterClose()
	}
	// A late transaction that SUCCEEDED would satisfy a bare count while
	// showing none of the harm: the point is that the commit fails and the
	// chunk it was carving stays local and unmirrored.
	for i, e := range errs {
		if e == nil || !strings.Contains(e.Error(), "database is closed") {
			t.Errorf("late transaction [%d] = %v, want a closed-database failure", i, e)
		}
	}
	for i, e := range errs {
		t.Logf("  [%d] %v", i, e)
	}
	if len(errs) == 0 {
		t.Fatal("no transaction reached the closed metadata store: the carve dispatcher never ran, so the guard proves nothing")
	}
}
