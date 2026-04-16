package storebackups

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/marmos91/dittofs/pkg/backup"
	"github.com/marmos91/dittofs/pkg/backup/destination"
	"github.com/marmos91/dittofs/pkg/backup/manifest"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/stores"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// ---- Restore test fakes ----

// fakeShares implements SharesService with a programmable enabled list.
type fakeShares struct {
	mu      sync.Mutex
	enabled map[string][]string // storeName -> share names
}

func (f *fakeShares) ListEnabledSharesForStore(name string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := f.enabled[name]; ok {
		out := make([]string, len(v))
		copy(out, v)
		return out
	}
	return nil
}

// fakeStoresService is a stub implementation of restore.StoresService. Used
// when RunRestore is expected to fail pre-flight — the restore executor is
// never reached, so the methods return errors.
type fakeStoresService struct {
	openErr   error
	swapErr   error
	dropCalls []string
	openStore metadata.MetadataStore
	swapOld   metadata.MetadataStore
}

func (f *fakeStoresService) OpenMetadataStoreAtPath(ctx context.Context, cfg *models.MetadataStoreConfig, path string) (metadata.MetadataStore, error) {
	if f.openErr != nil {
		return nil, f.openErr
	}
	if f.openStore != nil {
		return f.openStore, nil
	}
	return memory.NewMemoryMetadataStoreWithDefaults(), nil
}

func (f *fakeStoresService) SwapMetadataStore(name string, newStore metadata.MetadataStore) (metadata.MetadataStore, error) {
	if f.swapErr != nil {
		return nil, f.swapErr
	}
	if f.swapOld != nil {
		return f.swapOld, nil
	}
	return memory.NewMemoryMetadataStoreWithDefaults(), nil
}

func (f *fakeStoresService) DropPostgresSchema(ctx context.Context, originalName, schemaName string) error {
	f.dropCalls = append(f.dropCalls, originalName+":"+schemaName)
	return nil
}

// fakeRestoreResolver is a programmable RestoreResolver.
type fakeRestoreResolver struct {
	src       backup.Backupable
	storeID   string
	storeKind string
	storeName string
	cfg       *models.MetadataStoreConfig
	err       error
}

func (f *fakeRestoreResolver) Resolve(ctx context.Context, kind, id string) (backup.Backupable, string, string, error) {
	if f.err != nil {
		return nil, "", "", f.err
	}
	return f.src, f.storeID, f.storeKind, nil
}

func (f *fakeRestoreResolver) ResolveWithName(ctx context.Context, kind, id string) (backup.Backupable, string, string, string, error) {
	if f.err != nil {
		return nil, "", "", "", f.err
	}
	return f.src, f.storeID, f.storeKind, f.storeName, nil
}

func (f *fakeRestoreResolver) ResolveCfg(ctx context.Context, kind, id string) (*models.MetadataStoreConfig, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.cfg, nil
}

var _ RestoreResolver = (*fakeRestoreResolver)(nil)

// restoreDestination is a fake destination tailored for restore tests.
// It records how many times GetManifestOnly/GetBackup were called and with
// which record IDs, so tests can assert delegation to the restore executor.
type restoreDestination struct {
	mu                  sync.Mutex
	getManifestCalls    []string
	getBackupCalls      []string
	closed              bool
	manifestToReturn    *manifest.Manifest
	manifestErrToReturn error
	payloadToReturn     io.ReadCloser
	payloadErrToReturn  error
	// putBlockCh, if non-nil, blocks PutBackup until closed or ctx done.
	putBlockCh chan struct{}
	putCalls   int32
}

func (d *restoreDestination) PutBackup(ctx context.Context, m *manifest.Manifest, payload io.Reader) error {
	atomic.AddInt32(&d.putCalls, 1)
	if payload != nil {
		_, _ = io.Copy(io.Discard, payload)
	}
	d.mu.Lock()
	blockCh := d.putBlockCh
	d.mu.Unlock()
	if blockCh != nil {
		select {
		case <-blockCh:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	m.SizeBytes = 1
	m.SHA256 = "deadbeef"
	return nil
}

func (d *restoreDestination) GetBackup(ctx context.Context, id string) (*manifest.Manifest, io.ReadCloser, error) {
	d.mu.Lock()
	d.getBackupCalls = append(d.getBackupCalls, id)
	d.mu.Unlock()
	if d.payloadErrToReturn != nil {
		return nil, nil, d.payloadErrToReturn
	}
	return d.manifestToReturn, d.payloadToReturn, nil
}

func (d *restoreDestination) GetManifestOnly(ctx context.Context, id string) (*manifest.Manifest, error) {
	d.mu.Lock()
	d.getManifestCalls = append(d.getManifestCalls, id)
	d.mu.Unlock()
	if d.manifestErrToReturn != nil {
		return nil, d.manifestErrToReturn
	}
	return d.manifestToReturn, nil
}

func (d *restoreDestination) List(ctx context.Context) ([]destination.BackupDescriptor, error) {
	return nil, nil
}

func (d *restoreDestination) Stat(ctx context.Context, id string) (*destination.BackupDescriptor, error) {
	return nil, nil
}

func (d *restoreDestination) Delete(ctx context.Context, id string) error   { return nil }
func (d *restoreDestination) ValidateConfig(ctx context.Context) error      { return nil }
func (d *restoreDestination) Close() error {
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
	return nil
}

var _ destination.Destination = (*restoreDestination)(nil)

// ---- Restore test harness ----

type restoreHarness struct {
	svc      *Service
	shares   *fakeShares
	stores   *fakeStoresService
	resolver *fakeRestoreResolver
	dst      *restoreDestination
	repoID   string
	repo     *models.BackupRepo
}

func newRestoreHarness(t *testing.T, sharesEnabled map[string][]string) *restoreHarness {
	t.Helper()
	ctx := context.Background()

	cp := newTestStore(t)

	// Seed a repo (no schedule needed for restore tests).
	repo := &models.BackupRepo{
		TargetID:   "cfg-meta",
		TargetKind: "metadata",
		Name:       "default-restore",
		Kind:       models.BackupRepoKindLocal,
	}
	id, err := cp.CreateBackupRepo(ctx, repo)
	if err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	repo.ID = id

	shSvc := &fakeShares{enabled: sharesEnabled}
	stSvc := &fakeStoresService{}
	mem := memory.NewMemoryMetadataStoreWithDefaults()
	cfg := &models.MetadataStoreConfig{
		ID:     "cfg-meta",
		Name:   "default-meta",
		Type:   "memory",
	}
	resolver := &fakeRestoreResolver{
		src:       mem,
		storeID:   mem.GetStoreID(),
		storeKind: "memory",
		storeName: "default-meta",
		cfg:       cfg,
	}
	dst := &restoreDestination{}
	factory := func(ctx context.Context, r *models.BackupRepo) (destination.Destination, error) {
		return dst, nil
	}
	svc := New(cp, resolver, 500*time.Millisecond,
		WithDestinationFactory(factory),
		WithShares(shSvc),
		WithStores(stSvc),
	)
	t.Cleanup(func() { _ = svc.Stop(context.Background()) })
	return &restoreHarness{
		svc:      svc,
		shares:   shSvc,
		stores:   stSvc,
		resolver: resolver,
		dst:      dst,
		repoID:   id,
		repo:     repo,
	}
}

// seedSucceededRecord inserts a succeeded BackupRecord for h.repo with the
// given createdAt.
func (h *restoreHarness) seedSucceededRecord(t *testing.T, createdAt time.Time) *models.BackupRecord {
	t.Helper()
	return h.seedRecord(t, h.repoID, models.BackupStatusSucceeded, createdAt)
}

func (h *restoreHarness) seedRecord(t *testing.T, repoID string, status models.BackupStatus, createdAt time.Time) *models.BackupRecord {
	t.Helper()
	ctx := context.Background()
	rec := &models.BackupRecord{
		RepoID:    repoID,
		Status:    status,
		CreatedAt: createdAt,
		SizeBytes: 1,
		SHA256:    "deadbeef",
	}
	s := h.svc.store
	id, err := s.CreateBackupRecord(ctx, rec)
	if err != nil {
		t.Fatalf("seed record: %v", err)
	}
	rec.ID = id
	return rec
}

// ---- Tests ----

// TestRunRestore_SharesStillEnabled — REST-02 pre-flight gate rejects when
// the target store has enabled shares. The resolver is reached (we need
// the storeName) but the executor is NOT (no GetManifestOnly call).
func TestRunRestore_SharesStillEnabled(t *testing.T) {
	h := newRestoreHarness(t, map[string][]string{
		"default-meta": {"/foo", "/bar"},
	})
	err := h.svc.RunRestore(context.Background(), h.repoID, nil)
	if err == nil {
		t.Fatal("expected ErrRestorePreconditionFailed, got nil")
	}
	if !errors.Is(err, ErrRestorePreconditionFailed) {
		t.Fatalf("expected ErrRestorePreconditionFailed-wrapped, got %v", err)
	}
	if !strings.Contains(err.Error(), "/foo") || !strings.Contains(err.Error(), "/bar") {
		t.Errorf("expected share names in error message, got %q", err.Error())
	}
	h.dst.mu.Lock()
	n := len(h.dst.getManifestCalls)
	h.dst.mu.Unlock()
	if n != 0 {
		t.Errorf("expected zero GetManifestOnly calls (pre-flight should fail), got %d", n)
	}
}

// TestRunRestore_OverlapGuard — a RunBackup in flight on the same repo
// causes RunRestore to fail with ErrBackupAlreadyRunning (D-07 shared mutex).
func TestRunRestore_OverlapGuard(t *testing.T) {
	h := newRestoreHarness(t, nil)
	// Hold the overlap guard externally by pretending a backup is in flight.
	unlock, acquired := h.svc.overlap.TryLock(h.repoID)
	if !acquired {
		t.Fatal("failed to take overlap lock for test setup")
	}
	defer unlock()

	err := h.svc.RunRestore(context.Background(), h.repoID, nil)
	if err == nil {
		t.Fatal("expected ErrBackupAlreadyRunning, got nil")
	}
	if !errors.Is(err, ErrBackupAlreadyRunning) {
		t.Fatalf("expected ErrBackupAlreadyRunning-wrapped, got %v", err)
	}
}

// TestRunRestore_DefaultLatest — recordID=nil selects the most recent
// succeeded BackupRecord (D-15). We verify by observing which record ID
// the destination's GetManifestOnly was called with.
func TestRunRestore_DefaultLatest(t *testing.T) {
	h := newRestoreHarness(t, nil)

	// Seed three succeeded records; newest first in query result.
	base := time.Now().Add(-3 * time.Hour)
	h.seedSucceededRecord(t, base)
	h.seedSucceededRecord(t, base.Add(time.Hour))
	newest := h.seedSucceededRecord(t, base.Add(2*time.Hour))

	// Destination errors out right after manifest fetch — we only need to
	// observe that the manifest was requested for the newest record ID.
	h.dst.manifestErrToReturn = fmt.Errorf("stop-here: test-induced")

	err := h.svc.RunRestore(context.Background(), h.repoID, nil)
	if err == nil {
		t.Fatal("expected error (test-induced stop), got nil")
	}
	if !strings.Contains(err.Error(), "stop-here") {
		t.Fatalf("expected test-induced error, got %v", err)
	}

	h.dst.mu.Lock()
	calls := append([]string(nil), h.dst.getManifestCalls...)
	h.dst.mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("expected 1 GetManifestOnly call, got %d", len(calls))
	}
	if calls[0] != newest.ID {
		t.Errorf("expected latest record %q, got %q", newest.ID, calls[0])
	}
}

// TestRunRestore_NoSucceededRecords — recordID=nil with zero succeeded
// records surfaces ErrNoRestoreCandidate (D-15 empty case).
func TestRunRestore_NoSucceededRecords(t *testing.T) {
	h := newRestoreHarness(t, nil)

	// Seed only a failed record — it must NOT be selectable for restore.
	h.seedRecord(t, h.repoID, models.BackupStatusFailed, time.Now())

	err := h.svc.RunRestore(context.Background(), h.repoID, nil)
	if err == nil {
		t.Fatal("expected ErrNoRestoreCandidate, got nil")
	}
	if !errors.Is(err, ErrNoRestoreCandidate) {
		t.Fatalf("expected ErrNoRestoreCandidate-wrapped, got %v", err)
	}
}

// TestRunRestore_ByID_RepoMismatch — recordID belongs to a different repo
// (D-16) → ErrRecordRepoMismatch before the executor is touched.
func TestRunRestore_ByID_RepoMismatch(t *testing.T) {
	h := newRestoreHarness(t, nil)

	// Seed a record belonging to a DIFFERENT repo.
	otherRepo := &models.BackupRepo{
		TargetID:   "cfg-meta",
		TargetKind: "metadata",
		Name:       "other-repo",
		Kind:       models.BackupRepoKindLocal,
	}
	otherID, err := h.svc.store.CreateBackupRepo(context.Background(), otherRepo)
	if err != nil {
		t.Fatalf("seed other repo: %v", err)
	}
	rec := h.seedRecord(t, otherID, models.BackupStatusSucceeded, time.Now())

	err = h.svc.RunRestore(context.Background(), h.repoID, &rec.ID)
	if err == nil {
		t.Fatal("expected ErrRecordRepoMismatch, got nil")
	}
	if !errors.Is(err, ErrRecordRepoMismatch) {
		t.Fatalf("expected ErrRecordRepoMismatch-wrapped, got %v", err)
	}
}

// TestRunRestore_ByID_NotRestorable — recordID with status != succeeded
// (D-16) → ErrRecordNotRestorable.
func TestRunRestore_ByID_NotRestorable(t *testing.T) {
	h := newRestoreHarness(t, nil)

	rec := h.seedRecord(t, h.repoID, models.BackupStatusFailed, time.Now())

	err := h.svc.RunRestore(context.Background(), h.repoID, &rec.ID)
	if err == nil {
		t.Fatal("expected ErrRecordNotRestorable, got nil")
	}
	if !errors.Is(err, ErrRecordNotRestorable) {
		t.Fatalf("expected ErrRecordNotRestorable-wrapped, got %v", err)
	}
}

// TestRunRestore_HappyPath_DelegatesToExecutor — with valid pre-flight, the
// executor is invoked with a manifest fetch for the selected record. We
// assert the delegation by observing exactly one GetManifestOnly call with
// the expected record ID — this proves Service.RunRestore built Params
// correctly and handed off to restore.Executor.RunRestore.
func TestRunRestore_HappyPath_DelegatesToExecutor(t *testing.T) {
	h := newRestoreHarness(t, nil)

	rec := h.seedSucceededRecord(t, time.Now())

	// Program the destination to return a manifest whose store_id mismatches
	// (so the executor aborts before the side-engine open path). We just
	// need to reach the executor and confirm delegation; full end-to-end
	// path is exercised by pkg/backup/restore unit tests.
	h.dst.manifestToReturn = &manifest.Manifest{
		ManifestVersion: manifest.CurrentVersion,
		BackupID:        rec.ID,
		CreatedAt:       time.Now(),
		StoreID:         "mismatched-store-id",
		StoreKind:       "memory",
		SHA256:          "deadbeef",
		SizeBytes:       1,
		PayloadIDSet:    []string{},
	}

	err := h.svc.RunRestore(context.Background(), h.repoID, nil)
	if err == nil {
		t.Fatal("expected manifest validation error, got nil")
	}
	if !errors.Is(err, ErrStoreIDMismatch) {
		t.Fatalf("expected ErrStoreIDMismatch-wrapped, got %v", err)
	}

	h.dst.mu.Lock()
	calls := append([]string(nil), h.dst.getManifestCalls...)
	closed := h.dst.closed
	h.dst.mu.Unlock()

	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 GetManifestOnly call, got %d", len(calls))
	}
	if calls[0] != rec.ID {
		t.Errorf("expected record %q, got %q", rec.ID, calls[0])
	}
	if !closed {
		t.Error("expected destination Close() to be called on defer")
	}
}

// TestRunRestore_NotWired — Service constructed without WithShares/WithStores
// refuses RunRestore with a clear error (guards against silent no-op).
func TestRunRestore_NotWired(t *testing.T) {
	s := newTestStore(t)
	repo := &models.BackupRepo{
		TargetID:   "cfg-meta",
		TargetKind: "metadata",
		Name:       "unwired",
		Kind:       models.BackupRepoKindLocal,
	}
	id, err := s.CreateBackupRepo(context.Background(), repo)
	if err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	svc := New(s, &fakeRestoreResolver{}, 500*time.Millisecond)
	t.Cleanup(func() { _ = svc.Stop(context.Background()) })
	err = svc.RunRestore(context.Background(), id, nil)
	if err == nil {
		t.Fatal("expected 'restore path not wired' error, got nil")
	}
	if !strings.Contains(err.Error(), "restore path not wired") {
		t.Errorf("expected not-wired error, got %v", err)
	}
}

// ---- Orphan sweep tests ----

// fakeConfigLister is a programmable MetadataStoreConfigLister.
type fakeConfigLister struct {
	cfgs []*models.MetadataStoreConfig
	err  error
}

func (f *fakeConfigLister) ListMetadataStores(ctx context.Context) ([]*models.MetadataStoreConfig, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.cfgs, nil
}

// fakeOrphanLister is a programmable PostgresOrphanLister.
type fakeOrphanLister struct {
	mu        sync.Mutex
	orphans   map[string][]stores.PostgresRestoreOrphan // store name -> orphans
	dropCalls []string                                  // "store:schema" records
}

func (f *fakeOrphanLister) ListPostgresRestoreOrphans(ctx context.Context, originalName, schemaPrefix string) ([]stores.PostgresRestoreOrphan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.orphans[originalName], nil
}

func (f *fakeOrphanLister) DropPostgresSchema(ctx context.Context, originalName, schemaName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropCalls = append(f.dropCalls, originalName+":"+schemaName)
	return nil
}

// TestSweepRestoreOrphans_Badger creates a temp parent dir with two
// `.restore-<ulid>` entries — one backdated older than the grace window,
// one fresh. SweepRestoreOrphans must remove only the old one.
func TestSweepRestoreOrphans_Badger(t *testing.T) {
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "meta")
	if err := os.MkdirAll(storePath, 0o755); err != nil {
		t.Fatalf("mkdir store: %v", err)
	}

	oldULID := strings.ToLower(ulid.Make().String())
	oldOrphan := filepath.Join(tmp, "meta.restore-"+oldULID)
	if err := os.MkdirAll(oldOrphan, 0o755); err != nil {
		t.Fatalf("mkdir old orphan: %v", err)
	}
	// Back-date its mtime by 3 hours (grace is 1h default).
	oldTime := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(oldOrphan, oldTime, oldTime); err != nil {
		t.Fatalf("chtimes old: %v", err)
	}

	youngULID := strings.ToLower(ulid.Make().String())
	youngOrphan := filepath.Join(tmp, "meta.restore-"+youngULID)
	if err := os.MkdirAll(youngOrphan, 0o755); err != nil {
		t.Fatalf("mkdir young orphan: %v", err)
	}
	// Explicitly recent mtime.
	youngTime := time.Now().Add(-5 * time.Minute)
	if err := os.Chtimes(youngOrphan, youngTime, youngTime); err != nil {
		t.Fatalf("chtimes young: %v", err)
	}

	cfg := &models.MetadataStoreConfig{
		ID:   "cfg-badger",
		Name: "badger-meta",
		Type: "badger",
	}
	if err := cfg.SetConfig(map[string]any{"path": storePath}); err != nil {
		t.Fatalf("set cfg: %v", err)
	}

	lister := &fakeConfigLister{cfgs: []*models.MetadataStoreConfig{cfg}}
	orphanSvc := &fakeOrphanLister{}

	SweepRestoreOrphans(context.Background(), lister, orphanSvc, 1*time.Hour)

	// Old orphan must be gone.
	if _, err := os.Stat(oldOrphan); !os.IsNotExist(err) {
		t.Errorf("expected old orphan %q to be removed, stat=%v", oldOrphan, err)
	}
	// Young orphan must survive.
	if _, err := os.Stat(youngOrphan); err != nil {
		t.Errorf("expected young orphan %q preserved, got %v", youngOrphan, err)
	}
}

// TestSweepRestoreOrphans_PostgresCallsRequiredInterface ensures the
// Postgres branch calls stores.Service.ListPostgresRestoreOrphans +
// DropPostgresSchema directly (the plan's "required, not optional"
// contract). The fake PostgresOrphanLister records calls; we assert the
// old schema is dropped and the young one survives.
func TestSweepRestoreOrphans_PostgresCallsRequiredInterface(t *testing.T) {
	cfg := &models.MetadataStoreConfig{
		ID:   "cfg-pg",
		Name: "pg-meta",
		Type: "postgres",
	}
	if err := cfg.SetConfig(map[string]any{"schema": "public"}); err != nil {
		t.Fatalf("set cfg: %v", err)
	}

	oldTime := time.Now().Add(-3 * time.Hour)
	youngTime := time.Now().Add(-5 * time.Minute)

	lister := &fakeConfigLister{cfgs: []*models.MetadataStoreConfig{cfg}}
	orphanSvc := &fakeOrphanLister{
		orphans: map[string][]stores.PostgresRestoreOrphan{
			"pg-meta": {
				{Name: "public_restore_old", CreatedAt: oldTime},
				{Name: "public_restore_young", CreatedAt: youngTime},
			},
		},
	}

	SweepRestoreOrphans(context.Background(), lister, orphanSvc, 1*time.Hour)

	if len(orphanSvc.dropCalls) != 1 {
		t.Fatalf("expected 1 DropPostgresSchema call, got %d (%v)",
			len(orphanSvc.dropCalls), orphanSvc.dropCalls)
	}
	if orphanSvc.dropCalls[0] != "pg-meta:public_restore_old" {
		t.Errorf("unexpected drop call %q", orphanSvc.dropCalls[0])
	}
}

// TestSweepRestoreOrphans_MemoryNoOp — memory configs produce no sweep activity.
func TestSweepRestoreOrphans_MemoryNoOp(t *testing.T) {
	cfg := &models.MetadataStoreConfig{
		ID:   "cfg-mem",
		Name: "mem-meta",
		Type: "memory",
	}
	if err := cfg.SetConfig(map[string]any{}); err != nil {
		t.Fatalf("set cfg: %v", err)
	}

	lister := &fakeConfigLister{cfgs: []*models.MetadataStoreConfig{cfg}}
	orphanSvc := &fakeOrphanLister{}

	SweepRestoreOrphans(context.Background(), lister, orphanSvc, 1*time.Hour)

	if len(orphanSvc.dropCalls) != 0 {
		t.Errorf("memory config must not trigger any drop calls, got %v", orphanSvc.dropCalls)
	}
}

// TestSAFETY02_RestoreKindJobsRecovered — writes a BackupJob{Kind: restore,
// Status: running} directly into the store, invokes RecoverInterruptedJobs
// (via Service.Serve → s.store.RecoverInterruptedJobs), and asserts the row
// transitions to Status=interrupted. This verifies the Phase-1 recovery
// primitive handles restore-kind jobs uniformly with backup-kind — the
// SAFETY-02 extension.
func TestSAFETY02_RestoreKindJobsRecovered(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Seed a "running" restore job — no worker will ever finish it.
	startedAt := time.Now().Add(-5 * time.Minute)
	_, err := s.CreateBackupJob(ctx, &models.BackupJob{
		Kind:      models.BackupJobKindRestore,
		RepoID:    "some-repo-id",
		Status:    models.BackupStatusRunning,
		StartedAt: &startedAt,
	})
	if err != nil {
		t.Fatalf("seed running restore job: %v", err)
	}

	// Serve triggers RecoverInterruptedJobs.
	svc := New(s, &fakeRestoreResolver{}, 100*time.Millisecond)
	t.Cleanup(func() { _ = svc.Stop(context.Background()) })
	if err := svc.Serve(ctx); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	// The restore-kind job should now be interrupted.
	interrupted, err := s.ListBackupJobs(ctx, models.BackupJobKindRestore, models.BackupStatusInterrupted)
	if err != nil {
		t.Fatalf("list interrupted restore jobs: %v", err)
	}
	if len(interrupted) != 1 {
		t.Fatalf("expected 1 interrupted restore job, got %d", len(interrupted))
	}
	if interrupted[0].Kind != models.BackupJobKindRestore {
		t.Errorf("expected Kind=restore, got %q", interrupted[0].Kind)
	}
	if interrupted[0].Error == "" {
		t.Error("expected non-empty Error on interrupted restore job")
	}
	// No running restore jobs should remain.
	running, err := s.ListBackupJobs(ctx, models.BackupJobKindRestore, models.BackupStatusRunning)
	if err != nil {
		t.Fatalf("list running restore jobs: %v", err)
	}
	if len(running) != 0 {
		t.Errorf("expected 0 running restore jobs after recovery, got %d", len(running))
	}
}
