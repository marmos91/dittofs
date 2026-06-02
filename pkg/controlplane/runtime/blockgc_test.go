package runtime

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/remote"
	"github.com/marmos91/dittofs/pkg/health"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// ---- Fakes ----

// fakeRemoteStore is a minimal remote.RemoteStore for pointer-identity
// assertions in the dedup test.
type fakeRemoteStore struct {
	name string
}

// All methods on remote.RemoteStore. The fake is identity-only — it
// never actually performs I/O; tests assert on pointer identity and
// dedup wiring, not on byte-level behavior.
func (f *fakeRemoteStore) Put(_ context.Context, _ block.ContentHash, _ []byte) error {
	return nil
}
func (f *fakeRemoteStore) Get(_ context.Context, _ block.ContentHash) ([]byte, error) {
	return nil, nil
}
func (f *fakeRemoteStore) GetRange(_ context.Context, _ block.ContentHash, _, _ int64) ([]byte, error) {
	return nil, nil
}
func (f *fakeRemoteStore) Has(_ context.Context, _ block.ContentHash) (bool, error) {
	return false, nil
}
func (f *fakeRemoteStore) Delete(_ context.Context, _ block.ContentHash) error { return nil }
func (f *fakeRemoteStore) Head(_ context.Context, _ block.ContentHash) (block.Meta, error) {
	return block.Meta{}, nil
}
func (f *fakeRemoteStore) Walk(_ context.Context, _ func(block.ContentHash, block.Meta) error) error {
	return nil
}
func (f *fakeRemoteStore) ReadBlockVerified(_ context.Context, _ block.ContentHash, _ block.ContentHash) ([]byte, error) {
	return nil, nil
}
func (f *fakeRemoteStore) HealthCheck(_ context.Context) error { return nil }
func (f *fakeRemoteStore) Healthcheck(_ context.Context) health.Report {
	return health.Report{Status: health.StatusHealthy}
}
func (f *fakeRemoteStore) Close() error { return nil }

// installCollectGarbageSpy replaces collectGarbageFn with a capturing spy
// and registers automatic restoration via t.Cleanup. Returned slice pointer
// collects every invocation's *engine.Options so tests can assert on the
// DryRun / SharePrefix contract.
func installCollectGarbageSpy(t *testing.T) *[]*engine.Options {
	t.Helper()
	captured := make([]*engine.Options, 0, 4)
	orig := collectGarbageFn
	collectGarbageFn = func(_ context.Context, _ remote.RemoteStore, _ engine.MetadataReconciler, opts *engine.Options) *engine.GCStats {
		captured = append(captured, opts)
		return &engine.GCStats{}
	}
	t.Cleanup(func() { collectGarbageFn = orig })
	return &captured
}

// ---- Helpers ----

// newRuntimeForGC builds a Runtime fixture for RunBlockGC tests. Each entry
// in shareRemotes is added as a share with its remote store injected
// post-AddShare via the test-only setShareRemoteForTest helper.
func newRuntimeForGC(t *testing.T, shareRemotes map[string]remote.RemoteStore) *Runtime {
	t.Helper()
	rt := New(nil)
	ctx := context.Background()

	// Real memory metadata store keeps AddShare happy without needing a fake
	// with the full MetadataStore surface.
	metaStore := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	if err := rt.RegisterMetadataStore("meta", metaStore); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}

	for name, rs := range shareRemotes {
		cfg := &ShareConfig{Name: name, MetadataStore: "meta", Enabled: true}
		if err := rt.AddShare(ctx, cfg); err != nil {
			t.Fatalf("AddShare(%s): %v", name, err)
		}
		rt.setShareRemoteForTest(name, rs)
	}

	return rt
}

// ---- Tests ----

// TestRunBlockGC_DedupesSharedRemoteStores asserts that two shares sharing
// the same underlying remote result in exactly one CollectGarbage call.
func TestRunBlockGC_DedupesSharedRemoteStores(t *testing.T) {
	captured := installCollectGarbageSpy(t)

	sharedRS := &fakeRemoteStore{name: "s3-shared"}
	rt := newRuntimeForGC(t, map[string]remote.RemoteStore{
		"/share-a": sharedRS,
		"/share-b": sharedRS, // same pointer
	})

	if _, err := rt.RunBlockGC(context.Background(), "", false); err != nil {
		t.Fatalf("RunBlockGC: %v", err)
	}
	if len(*captured) != 1 {
		t.Fatalf("expected 1 deduped GC call, got %d", len(*captured))
	}
}

// TestRunBlockGC_DryRunPropagates asserts dryRun flows into the Options
// struct passed to CollectGarbage. engine.Options.SharePrefix was
// removed because the mark-sweep design has a global live set; the
// historical sharePrefix argument on RunBlockGC is preserved for
// caller compatibility but ignored.
func TestRunBlockGC_DryRunPropagates(t *testing.T) {
	captured := installCollectGarbageSpy(t)

	rs := &fakeRemoteStore{name: "s3-a"}
	rt := newRuntimeForGC(t, map[string]remote.RemoteStore{
		"/share-a": rs,
	})

	if _, err := rt.RunBlockGC(context.Background(), "/prefix", true); err != nil {
		t.Fatalf("RunBlockGC: %v", err)
	}
	if len(*captured) != 1 {
		t.Fatalf("expected 1 GC call, got %d", len(*captured))
	}
	if !(*captured)[0].DryRun {
		t.Fatal("expected DryRun=true on captured Options")
	}
}

// TestRunBlockGC_NoRemoteShares asserts RunBlockGC returns empty stats without
// error when no remote-backed shares are registered.
func TestRunBlockGC_NoRemoteShares(t *testing.T) {
	captured := installCollectGarbageSpy(t)

	rt := newRuntimeForGC(t, nil)

	stats, err := rt.RunBlockGC(context.Background(), "", false)
	if err != nil {
		t.Fatalf("RunBlockGC: %v", err)
	}
	if stats == nil {
		t.Fatal("expected non-nil stats")
	}
	if len(*captured) != 0 {
		t.Fatalf("expected 0 GC calls with no remote shares, got %d", len(*captured))
	}
}
