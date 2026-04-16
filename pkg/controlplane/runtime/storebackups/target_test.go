package storebackups

import (
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/backup"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// fakeConfigGetter is an in-memory MetadataStoreConfigGetter.
type fakeConfigGetter struct {
	byID map[string]*models.MetadataStoreConfig
}

func (f *fakeConfigGetter) GetMetadataStoreByID(ctx context.Context, id string) (*models.MetadataStoreConfig, error) {
	if c, ok := f.byID[id]; ok {
		return c, nil
	}
	return nil, models.ErrStoreNotFound
}

// fakeRegistry is an in-memory MetadataStoreRegistry.
type fakeRegistry struct {
	byName map[string]metadata.MetadataStore
}

func (f *fakeRegistry) GetMetadataStore(name string) (metadata.MetadataStore, error) {
	if m, ok := f.byName[name]; ok {
		return m, nil
	}
	return nil, errors.New("metadata store not loaded: " + name)
}

// TestBackupRepoTarget_ID asserts BackupRepoTarget.ID() returns the wrapped repo's ID.
func TestBackupRepoTarget_ID(t *testing.T) {
	sched := "0 * * * *"
	repo := &models.BackupRepo{ID: "r1", Schedule: &sched}
	target := NewBackupRepoTarget(repo)
	if got := target.ID(); got != "r1" {
		t.Fatalf("ID() = %q, want %q", got, "r1")
	}
}

// TestBackupRepoTarget_Schedule covers both non-nil and nil schedule.
func TestBackupRepoTarget_Schedule(t *testing.T) {
	t.Run("non_nil_schedule", func(t *testing.T) {
		sched := "*/5 * * * *"
		target := NewBackupRepoTarget(&models.BackupRepo{ID: "r1", Schedule: &sched})
		if got := target.Schedule(); got != "*/5 * * * *" {
			t.Fatalf("Schedule() = %q, want %q", got, "*/5 * * * *")
		}
	})
	t.Run("nil_schedule_returns_empty", func(t *testing.T) {
		target := NewBackupRepoTarget(&models.BackupRepo{ID: "r1", Schedule: nil})
		if got := target.Schedule(); got != "" {
			t.Fatalf("Schedule() for nil field = %q, want empty", got)
		}
	})
}

// TestBackupRepoTarget_Repo asserts the accessor returns the underlying repo.
func TestBackupRepoTarget_Repo(t *testing.T) {
	sched := "0 0 * * *"
	repo := &models.BackupRepo{ID: "r1", Schedule: &sched}
	target := NewBackupRepoTarget(repo)
	if target.Repo() != repo {
		t.Fatal("Repo() should return the wrapped repo pointer")
	}
}

// TestDefaultResolver_ResolveSuccess — T3 happy path: (metadata, cfgID) with backup-capable store.
func TestDefaultResolver_ResolveSuccess(t *testing.T) {
	ctx := context.Background()
	cfg := &models.MetadataStoreConfig{ID: "cfg-abc", Name: "test-meta", Type: "memory"}
	metaStore := memory.NewMemoryMetadataStoreWithDefaults()

	configs := &fakeConfigGetter{byID: map[string]*models.MetadataStoreConfig{"cfg-abc": cfg}}
	registry := &fakeRegistry{byName: map[string]metadata.MetadataStore{"test-meta": metaStore}}
	resolver := NewDefaultResolver(configs, registry)

	src, storeID, storeKind, err := resolver.Resolve(ctx, TargetKindMetadata, "cfg-abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if src == nil {
		t.Fatal("expected non-nil Backupable source")
	}
	if storeID != "cfg-abc" {
		t.Errorf("storeID = %q, want %q", storeID, "cfg-abc")
	}
	if storeKind != "memory" {
		t.Errorf("storeKind = %q, want %q", storeKind, "memory")
	}
}

// TestDefaultResolver_UnknownKind — T4: non-"metadata" kinds wrap ErrInvalidTargetKind.
func TestDefaultResolver_UnknownKind(t *testing.T) {
	ctx := context.Background()
	resolver := NewDefaultResolver(
		&fakeConfigGetter{byID: map[string]*models.MetadataStoreConfig{}},
		&fakeRegistry{byName: map[string]metadata.MetadataStore{}},
	)

	_, _, _, err := resolver.Resolve(ctx, "block", "whatever")
	if err == nil {
		t.Fatal("expected error for unknown kind")
	}
	if !errors.Is(err, ErrInvalidTargetKind) {
		t.Fatalf("expected ErrInvalidTargetKind-wrapped error, got %v", err)
	}
}

// TestDefaultResolver_ConfigMissing: "metadata" kind + unknown targetID wraps ErrStoreNotFound.
func TestDefaultResolver_ConfigMissing(t *testing.T) {
	ctx := context.Background()
	resolver := NewDefaultResolver(
		&fakeConfigGetter{byID: map[string]*models.MetadataStoreConfig{}},
		&fakeRegistry{byName: map[string]metadata.MetadataStore{}},
	)

	_, _, _, err := resolver.Resolve(ctx, TargetKindMetadata, "nope")
	if err == nil {
		t.Fatal("expected error when config is missing")
	}
	if !errors.Is(err, models.ErrStoreNotFound) {
		t.Fatalf("expected ErrStoreNotFound-wrapped error, got %v", err)
	}
}

// TestDefaultResolver_StoreNotRegistered: config present but runtime store not registered.
func TestDefaultResolver_StoreNotRegistered(t *testing.T) {
	ctx := context.Background()
	cfg := &models.MetadataStoreConfig{ID: "cfg-abc", Name: "orphan-meta", Type: "memory"}
	configs := &fakeConfigGetter{byID: map[string]*models.MetadataStoreConfig{"cfg-abc": cfg}}
	registry := &fakeRegistry{byName: map[string]metadata.MetadataStore{}}
	resolver := NewDefaultResolver(configs, registry)

	_, _, _, err := resolver.Resolve(ctx, TargetKindMetadata, "cfg-abc")
	if err == nil {
		t.Fatal("expected error when runtime store not registered")
	}
	if !errors.Is(err, models.ErrStoreNotFound) {
		t.Fatalf("expected ErrStoreNotFound-wrapped error, got %v", err)
	}
}

// TestErrorsAliasIdentity — storebackups.Err* sentinels share identity with models.Err*
// so errors.Is matches across the package boundary (supports Phase 6 API handlers
// importing either package).
func TestErrorsAliasIdentity(t *testing.T) {
	cases := []struct {
		name string
		pkg  error
		mdl  error
	}{
		{"ErrScheduleInvalid", ErrScheduleInvalid, models.ErrScheduleInvalid},
		{"ErrRepoNotFound", ErrRepoNotFound, models.ErrRepoNotFound},
		{"ErrBackupAlreadyRunning", ErrBackupAlreadyRunning, models.ErrBackupAlreadyRunning},
		{"ErrInvalidTargetKind", ErrInvalidTargetKind, models.ErrInvalidTargetKind},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.pkg != tc.mdl {
				t.Errorf("%s: storebackups sentinel must alias the models sentinel", tc.name)
			}
			// Also guard compile-time that backup.ErrBackupUnsupported is referenced
			// by target.go — if it changes identity, the type assertion path breaks.
			_ = backup.ErrBackupUnsupported
		})
	}
}
