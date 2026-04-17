package runtime

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/backup/destination"
	"github.com/marmos91/dittofs/pkg/backup/manifest"
	"github.com/marmos91/dittofs/pkg/blockstore/gc"
	"github.com/marmos91/dittofs/pkg/blockstore/remote"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/storebackups"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/health"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// ---- Fakes ----

// fakeRemoteStore is a minimal remote.RemoteStore for pointer-identity
// assertions in the dedup test.
type fakeRemoteStore struct {
	name string
}

func (f *fakeRemoteStore) WriteBlock(_ context.Context, _ string, _ []byte) error { return nil }
func (f *fakeRemoteStore) ReadBlock(_ context.Context, _ string) ([]byte, error)  { return nil, nil }
func (f *fakeRemoteStore) ReadBlockRange(_ context.Context, _ string, _, _ int64) ([]byte, error) {
	return nil, nil
}
func (f *fakeRemoteStore) DeleteBlock(_ context.Context, _ string) error    { return nil }
func (f *fakeRemoteStore) DeleteByPrefix(_ context.Context, _ string) error { return nil }
func (f *fakeRemoteStore) ListByPrefix(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}
func (f *fakeRemoteStore) CopyBlock(_ context.Context, _, _ string) error { return nil }
func (f *fakeRemoteStore) HealthCheck(_ context.Context) error            { return nil }
func (f *fakeRemoteStore) Healthcheck(_ context.Context) health.Report {
	return health.Report{Status: health.StatusHealthy}
}
func (f *fakeRemoteStore) Close() error { return nil }

// gcBlockBackupStore implements the subset of store.BackupStore that
// BackupHold needs for construction. HeldPayloadIDs is never invoked in
// these tests because the collectGarbageFn spy intercepts before GC would
// actually query the hold.
type gcBlockBackupStore struct {
	store.BackupStore // embed; methods not overridden panic if called
}

func (s *gcBlockBackupStore) ListAllBackupRepos(_ context.Context) ([]*models.BackupRepo, error) {
	return nil, nil
}

// destFactoryNoop produces a never-invoked destination — BackupHold path is
// not exercised by these tests because collectGarbageFn is stubbed.
func destFactoryNoop(_ context.Context, _ *models.BackupRepo) (destination.Destination, error) {
	return &fakeGCDestination{}, nil
}

type fakeGCDestination struct{}

func (d *fakeGCDestination) PutBackup(_ context.Context, _ *manifest.Manifest, _ io.Reader) error {
	return errors.New("not implemented")
}
func (d *fakeGCDestination) GetManifestOnly(_ context.Context, _ string) (*manifest.Manifest, error) {
	return nil, errors.New("not implemented")
}
func (d *fakeGCDestination) GetBackup(_ context.Context, _ string) (*manifest.Manifest, io.ReadCloser, error) {
	return nil, nil, errors.New("not implemented")
}
func (d *fakeGCDestination) List(_ context.Context) ([]destination.BackupDescriptor, error) {
	return nil, errors.New("not implemented")
}
func (d *fakeGCDestination) Stat(_ context.Context, _ string) (*destination.BackupDescriptor, error) {
	return nil, errors.New("not implemented")
}
func (d *fakeGCDestination) Delete(_ context.Context, _ string) error { return nil }
func (d *fakeGCDestination) ValidateConfig(_ context.Context) error   { return nil }
func (d *fakeGCDestination) Close() error                             { return nil }

// installCollectGarbageSpy replaces collectGarbageFn with a capturing spy
// and registers automatic restoration via t.Cleanup. Returned slice pointer
// collects every invocation's *gc.Options so tests can assert on the
// BackupHold / DryRun / SharePrefix contract.
func installCollectGarbageSpy(t *testing.T) *[]*gc.Options {
	t.Helper()
	captured := make([]*gc.Options, 0, 4)
	orig := collectGarbageFn
	collectGarbageFn = func(_ context.Context, _ remote.RemoteStore, _ gc.MetadataReconciler, opts *gc.Options) *gc.Stats {
		captured = append(captured, opts)
		return &gc.Stats{}
	}
	t.Cleanup(func() { collectGarbageFn = orig })
	return &captured
}

// ---- Helpers ----

// newRuntimeForGC builds a Runtime fixture for RunBlockGC tests. When
// withBackupHold is true, the runtime is populated with a fake BackupStore
// and destFactory so RunBlockGC's SAFETY-01 precondition passes. Each entry
// in shareRemotes is added as a share with its remote store injected
// post-AddShare via the test-only setShareRemoteForTest helper.
func newRuntimeForGC(t *testing.T, withBackupHold bool, shareRemotes map[string]remote.RemoteStore) *Runtime {
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

	if withBackupHold {
		rt.SetBackupHoldWiringForTest(&gcBlockBackupStore{}, storebackups.DestinationFactoryFn(destFactoryNoop))
	}

	return rt
}

// ---- Tests ----

// TestRunBlockGC_AttachesBackupHold asserts the SAFETY-01 invariant: every
// production GC invocation is called with Options.BackupHold populated.
func TestRunBlockGC_AttachesBackupHold(t *testing.T) {
	captured := installCollectGarbageSpy(t)

	rs := &fakeRemoteStore{name: "s3-a"}
	rt := newRuntimeForGC(t, true, map[string]remote.RemoteStore{
		"/share-a": rs,
	})

	stats, err := rt.RunBlockGC(context.Background(), "", false)
	if err != nil {
		t.Fatalf("RunBlockGC: %v", err)
	}
	if stats == nil {
		t.Fatal("expected non-nil stats")
	}
	if len(*captured) != 1 {
		t.Fatalf("expected 1 captured GC call, got %d", len(*captured))
	}
	if (*captured)[0].BackupHold == nil {
		t.Fatal("SAFETY-01: BackupHold must be attached on every production GC run")
	}
}

// TestRunBlockGC_MissingBackupStore_ReturnsError asserts RunBlockGC refuses
// to run without backup-hold wiring rather than silently under-holding.
func TestRunBlockGC_MissingBackupStore_ReturnsError(t *testing.T) {
	captured := installCollectGarbageSpy(t)

	rs := &fakeRemoteStore{name: "s3-a"}
	rt := newRuntimeForGC(t, false /* no backup-hold */, map[string]remote.RemoteStore{
		"/share-a": rs,
	})

	stats, err := rt.RunBlockGC(context.Background(), "", false)
	if err == nil {
		t.Fatal("expected error when backup-hold wiring is unavailable")
	}
	if stats != nil {
		t.Fatal("expected nil stats on refusal")
	}
	if !strings.Contains(err.Error(), "backup-hold wiring unavailable") {
		t.Fatalf("expected 'backup-hold wiring unavailable' in error; got: %v", err)
	}
	if len(*captured) != 0 {
		t.Fatalf("expected zero GC invocations on refusal; got %d", len(*captured))
	}
}

// TestRunBlockGC_DedupesSharedRemoteStores asserts that two shares sharing
// the same underlying remote result in exactly one CollectGarbage call.
func TestRunBlockGC_DedupesSharedRemoteStores(t *testing.T) {
	captured := installCollectGarbageSpy(t)

	sharedRS := &fakeRemoteStore{name: "s3-shared"}
	rt := newRuntimeForGC(t, true, map[string]remote.RemoteStore{
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

// TestRunBlockGC_DryRunPropagates asserts dryRun + sharePrefix flow into
// the Options struct passed to CollectGarbage, and that BackupHold is
// attached even on dry-run paths.
func TestRunBlockGC_DryRunPropagates(t *testing.T) {
	captured := installCollectGarbageSpy(t)

	rs := &fakeRemoteStore{name: "s3-a"}
	rt := newRuntimeForGC(t, true, map[string]remote.RemoteStore{
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
	if (*captured)[0].SharePrefix != "/prefix" {
		t.Fatalf("expected SharePrefix=\"/prefix\" on captured Options; got %q", (*captured)[0].SharePrefix)
	}
	if (*captured)[0].BackupHold == nil {
		t.Fatal("SAFETY-01: BackupHold must be attached even on dry-run")
	}
}
