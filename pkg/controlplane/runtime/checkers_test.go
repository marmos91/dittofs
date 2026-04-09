//go:build integration

package runtime

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/adapters"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/health"
	"github.com/marmos91/dittofs/pkg/metadata"
	memoryMeta "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newRuntimeForChecks returns a Runtime wired to an in-memory SQLite
// control-plane store. Tests in this file rely on a real store for
// the block store checker path; the other paths don't touch it.
func newRuntimeForChecks(t *testing.T) (*Runtime, store.Store) {
	t.Helper()
	cfg := store.Config{
		Type:   "sqlite",
		SQLite: store.SQLiteConfig{Path: ":memory:"},
	}
	cpStore, err := store.New(&cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	rt := New(cpStore)
	return rt, cpStore
}

// countingMetaStore wraps the real in-memory metadata store and
// counts calls to Healthcheck. It embeds the real store to inherit
// every other method of the large MetadataStore interface.
type countingMetaStore struct {
	metadata.MetadataStore
	calls int64
}

func newCountingMetaStore() *countingMetaStore {
	return &countingMetaStore{
		MetadataStore: memoryMeta.NewMemoryMetadataStoreWithDefaults(),
	}
}

func (c *countingMetaStore) Healthcheck(ctx context.Context) health.Report {
	atomic.AddInt64(&c.calls, 1)
	return c.MetadataStore.Healthcheck(ctx)
}

func (c *countingMetaStore) Count() int64 { return atomic.LoadInt64(&c.calls) }

// fakeAdapter is a minimal [adapters.ProtocolAdapter] implementation
// suitable for the adapter checker test. Only Healthcheck is
// exercised meaningfully; Serve blocks until ctx is canceled so
// AddAdapter's goroutine exits cleanly.
type fakeAdapter struct {
	protocol string
	calls    int64
	status   health.Status
	stopped  chan struct{}
}

func newFakeAdapter(protocol string, status health.Status) *fakeAdapter {
	return &fakeAdapter{protocol: protocol, status: status, stopped: make(chan struct{})}
}

func (a *fakeAdapter) Serve(ctx context.Context) error {
	<-ctx.Done()
	close(a.stopped)
	return ctx.Err()
}

func (a *fakeAdapter) Stop(_ context.Context) error { return nil }
func (a *fakeAdapter) Protocol() string             { return a.protocol }
func (a *fakeAdapter) Port() int                    { return 0 }
func (a *fakeAdapter) Healthcheck(_ context.Context) health.Report {
	atomic.AddInt64(&a.calls, 1)
	return health.Report{Status: a.status, CheckedAt: time.Now().UTC()}
}

func (a *fakeAdapter) Count() int64 { return atomic.LoadInt64(&a.calls) }

// compile-time check.
var _ adapters.ProtocolAdapter = (*fakeAdapter)(nil)

func TestStatusCheckers_MetadataStore_CachedAcrossCalls(t *testing.T) {
	rt, _ := newRuntimeForChecks(t)
	rt.statusCheckers = newCheckerCache(5 * time.Second)

	meta := newCountingMetaStore()
	if err := rt.RegisterMetadataStore("m1", meta); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}

	c := rt.MetadataStoreChecker("m1")
	_ = c.Healthcheck(context.Background())
	_ = c.Healthcheck(context.Background())
	_ = c.Healthcheck(context.Background())

	if got := meta.Count(); got != 1 {
		t.Errorf("expected 1 underlying Healthcheck call, got %d", got)
	}
}

func TestStatusCheckers_MetadataStore_WrapperIsCached(t *testing.T) {
	rt, _ := newRuntimeForChecks(t)
	meta := newCountingMetaStore()
	_ = rt.RegisterMetadataStore("m1", meta)

	first := rt.MetadataStoreChecker("m1")
	second := rt.MetadataStoreChecker("m1")
	if first != second {
		t.Errorf("MetadataStoreChecker returned different wrappers for the same name")
	}
}

func TestStatusCheckers_MetadataStore_UnknownReturnsNotLoaded(t *testing.T) {
	rt, _ := newRuntimeForChecks(t)
	c := rt.MetadataStoreChecker("nope")
	rep := c.Healthcheck(context.Background())
	if rep.Status != health.StatusUnknown {
		t.Errorf("status = %s, want unknown", rep.Status)
	}
	if !strings.Contains(rep.Message, "not loaded") {
		t.Errorf("message = %q, want it to mention 'not loaded'", rep.Message)
	}
}

func TestStatusCheckers_Adapter_NotRunningThenRunning(t *testing.T) {
	rt, _ := newRuntimeForChecks(t)

	// Not running → unknown
	rep := rt.AdapterChecker("fake").Healthcheck(context.Background())
	if rep.Status != health.StatusUnknown {
		t.Errorf("unregistered adapter status = %s, want unknown", rep.Status)
	}
	if !strings.Contains(rep.Message, "not running") {
		t.Errorf("message = %q, want 'not running'", rep.Message)
	}

	// The "not running" report was cached. Reset the cache to exercise
	// the running path without waiting for TTL.
	rt.statusCheckers = newCheckerCache(5 * time.Second)

	fa := newFakeAdapter("fake", health.StatusHealthy)
	if err := rt.AddAdapter(fa); err != nil {
		t.Fatalf("AddAdapter: %v", err)
	}
	t.Cleanup(func() { _ = rt.StopAllAdapters() })

	rep = rt.AdapterChecker("fake").Healthcheck(context.Background())
	if rep.Status != health.StatusHealthy {
		t.Errorf("running adapter status = %s, want healthy", rep.Status)
	}

	// A second call inside TTL must NOT re-invoke the adapter probe.
	_ = rt.AdapterChecker("fake").Healthcheck(context.Background())
	if got := fa.Count(); got != 1 {
		t.Errorf("expected 1 underlying Healthcheck call, got %d", got)
	}
}

func TestStatusCheckers_ShareChecker_CachesWorstOfProbe(t *testing.T) {
	rt, _ := newRuntimeForChecks(t)
	rt.statusCheckers = newCheckerCache(5 * time.Second)

	meta := newCountingMetaStore()
	if err := rt.RegisterMetadataStore("share-meta", meta); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}

	// Inject a share directly via shares.Service's test helper so we
	// don't need to drag block store / quota setup into this test.
	rt.sharesSvc.InjectShareForTesting(&shares.Share{
		Name:          "/s",
		MetadataStore: "share-meta",
	})

	c := rt.ShareChecker("/s")
	_ = c.Healthcheck(context.Background())
	_ = c.Healthcheck(context.Background())
	_ = c.Healthcheck(context.Background())

	if got := meta.Count(); got != 1 {
		t.Errorf("expected 1 underlying Healthcheck call, got %d", got)
	}
}

func TestStatusCheckers_BlockStore_UnknownWhenStoreMissing(t *testing.T) {
	rt, _ := newRuntimeForChecks(t)
	c := rt.BlockStoreChecker(models.BlockStoreKindLocal, "does-not-exist")
	rep := c.Healthcheck(context.Background())
	if rep.Status != health.StatusUnknown {
		t.Errorf("missing config status = %s, want unknown", rep.Status)
	}
	if !strings.Contains(rep.Message, "not found") {
		t.Errorf("message = %q, expected 'not found'", rep.Message)
	}
}

func TestStatusCheckers_BlockStore_MemoryStoreHealthy(t *testing.T) {
	rt, cpStore := newRuntimeForChecks(t)
	rt.statusCheckers = newCheckerCache(5 * time.Second)

	bs := &models.BlockStoreConfig{
		ID: uuid.New().String(), Name: "mem", Kind: models.BlockStoreKindLocal, Type: "memory",
		CreatedAt: time.Now(),
	}
	if _, err := cpStore.CreateBlockStore(context.Background(), bs); err != nil {
		t.Fatalf("CreateBlockStore: %v", err)
	}

	rep := rt.BlockStoreChecker(models.BlockStoreKindLocal, "mem").Healthcheck(context.Background())
	if rep.Status != health.StatusHealthy {
		t.Errorf("memory block store status = %s, want healthy (msg=%q)", rep.Status, rep.Message)
	}

	// Second call inside TTL still returns healthy; this just locks
	// in that the cached wrapper doesn't drift on repeat access.
	rep2 := rt.BlockStoreChecker(models.BlockStoreKindLocal, "mem").Healthcheck(context.Background())
	if rep2.Status != health.StatusHealthy {
		t.Errorf("second call status = %s, want healthy", rep2.Status)
	}
}

// mutableFakeAdapter is a [adapters.ProtocolAdapter] whose
// Healthcheck return value can be swapped at runtime. Used by the
// TTL expiry test to prove that a second call after TTL elapses
// produces a freshly probed report instead of the cached one.
type mutableFakeAdapter struct {
	protocol string
	calls    int64
	status   atomic.Value // health.Status
	stopped  chan struct{}
}

func newMutableFakeAdapter(protocol string, initial health.Status) *mutableFakeAdapter {
	a := &mutableFakeAdapter{protocol: protocol, stopped: make(chan struct{})}
	a.status.Store(initial)
	return a
}

func (a *mutableFakeAdapter) Serve(ctx context.Context) error {
	<-ctx.Done()
	close(a.stopped)
	return ctx.Err()
}

func (a *mutableFakeAdapter) Stop(_ context.Context) error { return nil }
func (a *mutableFakeAdapter) Protocol() string             { return a.protocol }
func (a *mutableFakeAdapter) Port() int                    { return 0 }
func (a *mutableFakeAdapter) Healthcheck(_ context.Context) health.Report {
	atomic.AddInt64(&a.calls, 1)
	return health.Report{Status: a.status.Load().(health.Status), CheckedAt: time.Now().UTC()}
}
func (a *mutableFakeAdapter) SetStatus(s health.Status) { a.status.Store(s) }
func (a *mutableFakeAdapter) Count() int64              { return atomic.LoadInt64(&a.calls) }

var _ adapters.ProtocolAdapter = (*mutableFakeAdapter)(nil)

// TestStatusCheckers_TTLExpiry proves the CachedChecker layer
// actually honors its TTL: a second Healthcheck call after the TTL
// window must produce a freshly probed report, not the cached one.
// The test swaps the underlying adapter's status between the two
// calls and asserts the second call sees the new value.
func TestStatusCheckers_TTLExpiry(t *testing.T) {
	rt, _ := newRuntimeForChecks(t)
	// Very short TTL so the test completes quickly. We sleep past
	// this window between the two Healthcheck calls.
	ttl := 10 * time.Millisecond
	rt.statusCheckers = newCheckerCache(ttl)

	fa := newMutableFakeAdapter("fake-ttl", health.StatusHealthy)
	if err := rt.AddAdapter(fa); err != nil {
		t.Fatalf("AddAdapter: %v", err)
	}
	t.Cleanup(func() { _ = rt.StopAllAdapters() })

	// First call: probes the adapter once and caches the healthy
	// report.
	rep1 := rt.AdapterChecker("fake-ttl").Healthcheck(context.Background())
	if rep1.Status != health.StatusHealthy {
		t.Fatalf("first call status = %s, want healthy", rep1.Status)
	}
	if got := fa.Count(); got != 1 {
		t.Fatalf("first call underlying probe count = %d, want 1", got)
	}

	// Flip the adapter's health and wait past the TTL window so the
	// CachedChecker discards its memoized report.
	fa.SetStatus(health.StatusUnhealthy)
	time.Sleep(ttl * 5)

	// Second call: MUST re-probe and observe the new status. If the
	// cache incorrectly returned the stale healthy report, this fails
	// with status=healthy and underlying call count still 1.
	rep2 := rt.AdapterChecker("fake-ttl").Healthcheck(context.Background())
	if rep2.Status != health.StatusUnhealthy {
		t.Errorf("second call status = %s, want unhealthy (cache did not expire)", rep2.Status)
	}
	if got := fa.Count(); got < 2 {
		t.Errorf("expected at least 2 underlying Healthcheck calls after TTL, got %d", got)
	}
}

func TestStatusCheckers_CanceledContextDoesNotPanic(t *testing.T) {
	rt, _ := newRuntimeForChecks(t)
	meta := newCountingMetaStore()
	_ = rt.RegisterMetadataStore("m1", meta)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// CachedChecker detaches the probe context, so a pre-canceled
	// caller ctx surfaces either as a completed probe (if fast) or
	// as a "canceled" unknown report (if the caller's select fires
	// first). Either way the call MUST NOT panic and MUST return a
	// populated Report.
	rep := rt.MetadataStoreChecker("m1").Healthcheck(ctx)
	if rep.Status == "" {
		t.Errorf("expected a populated status, got empty")
	}
}
