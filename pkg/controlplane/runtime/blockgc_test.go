package runtime

import (
	"context"
	"io"
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

// remote.RemoteBlockStore
func (f *fakeRemoteStore) PutBlock(_ context.Context, _ string, _ io.Reader) error { return nil }
func (f *fakeRemoteStore) GetBlock(_ context.Context, _ string) ([]byte, error)    { return nil, nil }
func (f *fakeRemoteStore) GetBlockRange(_ context.Context, _ string, _, _ int64) ([]byte, error) {
	return nil, nil
}
func (f *fakeRemoteStore) DeleteBlock(_ context.Context, _ string) error { return nil }
func (f *fakeRemoteStore) WalkBlocks(_ context.Context, _ func(string, block.Meta) error) error {
	return nil
}

// remote.ChunkReader / remote.ChunkSealer
func (f *fakeRemoteStore) ReadChunk(_ context.Context, _ string, _, _ int64, _ block.ContentHash) ([]byte, error) {
	return nil, nil
}
func (f *fakeRemoteStore) SealChunk(_ context.Context, _ block.ContentHash, plaintext []byte) ([]byte, error) {
	return plaintext, nil
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
	collectGarbageFn = func(_ context.Context, _ engine.MetadataReconciler, opts *engine.Options) *engine.GCStats {
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

	if _, err := rt.RunBlockGC(context.Background(), false); err != nil {
		t.Fatalf("RunBlockGC: %v", err)
	}
	if len(*captured) != 1 {
		t.Fatalf("expected 1 deduped GC call, got %d", len(*captured))
	}
}

// TestRunBlockGC_DryRunPropagates asserts dryRun flows into the Options
// struct passed to CollectGarbage.
func TestRunBlockGC_DryRunPropagates(t *testing.T) {
	captured := installCollectGarbageSpy(t)

	rs := &fakeRemoteStore{name: "s3-a"}
	rt := newRuntimeForGC(t, map[string]remote.RemoteStore{
		"/share-a": rs,
	})

	if _, err := rt.RunBlockGC(context.Background(), true); err != nil {
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

	stats, err := rt.RunBlockGC(context.Background(), false)
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

// TestRunBlockGC_PropagatesRemoteTierErrorDetail asserts the server-wide sweep
// carries a remote sweep's FirstErrors and dry-run candidates into the
// aggregate it returns, not just the ErrorCount. FirstErrors is the only cause
// detail dfsctl renders for a failed GC run, so dropping it leaves an operator
// with a bare "errors: N".
func TestRunBlockGC_PropagatesRemoteTierErrorDetail(t *testing.T) {
	orig := collectGarbageFn
	collectGarbageFn = func(_ context.Context, _ engine.MetadataReconciler, _ *engine.Options) *engine.GCStats {
		return &engine.GCStats{
			ErrorCount:       2,
			FirstErrors:      []string{"s3: connection refused", "s3: access denied"},
			DryRun:           true,
			DryRunCandidates: []string{"blocks/abc"},
		}
	}
	t.Cleanup(func() { collectGarbageFn = orig })

	rt := newRuntimeForGC(t, map[string]remote.RemoteStore{"/share-a": &fakeRemoteStore{name: "s3"}})

	stats, err := rt.RunBlockGC(context.Background(), true)
	if err != nil {
		t.Fatalf("RunBlockGC: %v", err)
	}
	if stats.ErrorCount != 2 {
		t.Errorf("ErrorCount = %d, want 2", stats.ErrorCount)
	}
	if len(stats.FirstErrors) != 2 {
		t.Fatalf("FirstErrors = %v, want the remote sweep's 2 entries", stats.FirstErrors)
	}
	if !stats.DryRun {
		t.Error("DryRun not propagated from the remote sweep")
	}
	if len(stats.DryRunCandidates) != 1 {
		t.Errorf("DryRunCandidates = %v, want 1 entry", stats.DryRunCandidates)
	}
}
