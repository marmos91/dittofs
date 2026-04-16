package restore

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/backup"
	"github.com/marmos91/dittofs/pkg/backup/destination"
	"github.com/marmos91/dittofs/pkg/backup/manifest"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// --- fakes ---------------------------------------------------------------

// fakeJobStore records every CreateBackupJob / UpdateBackupJob call so
// tests can assert the exact terminal state (succeeded / failed /
// interrupted) produced by RunRestore's defer.
type fakeJobStore struct {
	mu             sync.Mutex
	createdJobs    []models.BackupJob
	updatedJobs    []models.BackupJob
	createJobErr   error
	records        map[string]*models.BackupRecord
	recordsForRepo map[string][]*models.BackupRecord
}

func newFakeJobStore() *fakeJobStore {
	return &fakeJobStore{
		records:        make(map[string]*models.BackupRecord),
		recordsForRepo: make(map[string][]*models.BackupRecord),
	}
}

func (s *fakeJobStore) CreateBackupJob(ctx context.Context, j *models.BackupJob) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.createJobErr != nil {
		return "", s.createJobErr
	}
	s.createdJobs = append(s.createdJobs, *j)
	return j.ID, nil
}

func (s *fakeJobStore) UpdateBackupJob(ctx context.Context, j *models.BackupJob) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updatedJobs = append(s.updatedJobs, *j)
	return nil
}

func (s *fakeJobStore) GetBackupRecord(ctx context.Context, id string) (*models.BackupRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.records[id]; ok {
		return rec, nil
	}
	return nil, models.ErrBackupRecordNotFound
}

func (s *fakeJobStore) ListSucceededRecordsByRepo(ctx context.Context, repoID string) ([]*models.BackupRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recordsForRepo[repoID], nil
}

// finalStatus returns the last UpdateBackupJob call's Status field.
func (s *fakeJobStore) finalStatus() models.BackupStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.updatedJobs) == 0 {
		return ""
	}
	return s.updatedJobs[len(s.updatedJobs)-1].Status
}

// fakeDest is a destination.Destination double. getManifestFn /
// getBackupFn are programmable so tests can inject validation-gate
// mismatches + SHA-256 mismatch close errors.
type fakeDest struct {
	getManifestFn func(ctx context.Context, id string) (*manifest.Manifest, error)
	getBackupFn   func(ctx context.Context, id string) (*manifest.Manifest, io.ReadCloser, error)
}

func (d *fakeDest) PutBackup(ctx context.Context, m *manifest.Manifest, payload io.Reader) error {
	return nil
}

func (d *fakeDest) GetManifestOnly(ctx context.Context, id string) (*manifest.Manifest, error) {
	if d.getManifestFn != nil {
		return d.getManifestFn(ctx, id)
	}
	return nil, errors.New("fakeDest: GetManifestOnly not programmed")
}

func (d *fakeDest) GetBackup(ctx context.Context, id string) (*manifest.Manifest, io.ReadCloser, error) {
	if d.getBackupFn != nil {
		return d.getBackupFn(ctx, id)
	}
	return nil, nil, errors.New("fakeDest: GetBackup not programmed")
}

func (d *fakeDest) List(ctx context.Context) ([]destination.BackupDescriptor, error) {
	return nil, nil
}

func (d *fakeDest) Stat(ctx context.Context, id string) (*destination.BackupDescriptor, error) {
	return nil, nil
}

func (d *fakeDest) Delete(ctx context.Context, id string) error { return nil }
func (d *fakeDest) ValidateConfig(ctx context.Context) error    { return nil }
func (d *fakeDest) Close() error                                { return nil }

// programmableReader lets tests program the Close() return value to
// simulate Phase 3 D-11's SHA-256-verify-on-close semantics, and the
// Read() return value to simulate a ctx-canceled stream mid-transfer.
type programmableReader struct {
	mu       sync.Mutex
	data     []byte
	pos      int
	closed   bool
	closeErr error

	readErr error // returned by Read once pos >= abortAt
	abortAt int
}

func (r *programmableReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, io.ErrClosedPipe
	}
	if r.readErr != nil && r.pos >= r.abortAt {
		return 0, r.readErr
	}
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

func (r *programmableReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	return r.closeErr
}

// fakeStores implements StoresService. Records calls for assertions.
type fakeStores struct {
	mu sync.Mutex

	openCalls int
	openFn    func(ctx context.Context, cfg *models.MetadataStoreConfig, pathOverride string) (metadata.MetadataStore, error)
	swapCalls int
	swapFn    func(name string, newStore metadata.MetadataStore) (metadata.MetadataStore, error)
	dropCalls int
}

func (s *fakeStores) OpenMetadataStoreAtPath(
	ctx context.Context,
	cfg *models.MetadataStoreConfig,
	pathOverride string,
) (metadata.MetadataStore, error) {
	s.mu.Lock()
	s.openCalls++
	s.mu.Unlock()
	if s.openFn != nil {
		return s.openFn(ctx, cfg, pathOverride)
	}
	// Default: fresh memory store, which fully implements
	// metadata.MetadataStore + backup.Backupable.
	return memory.NewMemoryMetadataStoreWithDefaults(), nil
}

func (s *fakeStores) SwapMetadataStore(name string, newStore metadata.MetadataStore) (metadata.MetadataStore, error) {
	s.mu.Lock()
	s.swapCalls++
	s.mu.Unlock()
	if s.swapFn != nil {
		return s.swapFn(name, newStore)
	}
	return memory.NewMemoryMetadataStoreWithDefaults(), nil
}

func (s *fakeStores) DropPostgresSchema(ctx context.Context, originalName, schemaName string) error {
	s.mu.Lock()
	s.dropCalls++
	s.mu.Unlock()
	return nil
}

func (s *fakeStores) swapCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.swapCalls
}

func (s *fakeStores) openCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.openCalls
}

// fixedClock returns the same time on every call.
type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// --- helpers -------------------------------------------------------------

func newRepo() *models.BackupRepo {
	return &models.BackupRepo{
		ID:         "repo-1",
		TargetID:   "store-xyz",
		TargetKind: "metadata",
		Name:       "test-repo",
		Kind:       models.BackupRepoKindLocal,
	}
}

func newCfg() *models.MetadataStoreConfig {
	return &models.MetadataStoreConfig{
		ID:   "store-xyz",
		Name: "primary",
		Type: "memory",
	}
}

func validManifest() *manifest.Manifest {
	return &manifest.Manifest{
		ManifestVersion: manifest.CurrentVersion,
		BackupID:        "rec-1",
		CreatedAt:       time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		StoreID:         "store-xyz",
		StoreKind:       "memory",
		SHA256:          "deadbeef",
		SizeBytes:       5,
		PayloadIDSet:    []string{},
	}
}

// buildParams builds a Params object with a programmable BumpBootVerifier
// counter. Callers pass &bumpCalls to observe boot-verifier invocations.
func buildParams(d *fakeDest, ss *fakeStores, bumpCalls *int) Params {
	var bumpFn func()
	if bumpCalls != nil {
		bumpFn = func() { *bumpCalls++ }
	}
	return Params{
		Repo:             newRepo(),
		Dst:              d,
		RecordID:         "rec-1",
		TargetStoreKind:  "memory",
		TargetStoreID:    "store-xyz",
		TargetStoreCfg:   newCfg(),
		StoresService:    ss,
		BumpBootVerifier: bumpFn,
	}
}

// happyPathFakes wires a full run to success: manifest matches target
// identity, payload streams + closes cleanly, swap returns a fresh
// old-store for RunRestore to close.
func happyPathFakes() (*fakeJobStore, *fakeStores, *fakeDest) {
	ss := &fakeStores{} // defaults: fresh memory stores
	m := validManifest()
	d := &fakeDest{
		getManifestFn: func(ctx context.Context, id string) (*manifest.Manifest, error) {
			return m, nil
		},
		getBackupFn: func(ctx context.Context, id string) (*manifest.Manifest, io.ReadCloser, error) {
			// A nonsense payload is fine here — memory.Restore on an
			// empty store will surface ErrRestoreCorrupt because the
			// bytes aren't a valid memory-backup envelope. To keep
			// restore happy, use a pre-built real memory backup byte
			// stream. For simplicity, we bypass the real memory.Restore
			// path by having the stores.OpenFn return a store that
			// already tolerates arbitrary input: the default memory
			// store rejects, so the tests that rely on the happy-path
			// substitute a custom backupable via openFn. See TestRunRestore_HappyPath.
			return m, &programmableReader{data: []byte("ignored")}, nil
		},
	}
	return newFakeJobStore(), ss, d
}

// tolerantMemStore is a minimal wrapper around memory.MemoryMetadataStore
// that accepts any Restore stream as successful. Used in tests where
// we want to focus on the D-05 orchestration, not the engine's
// restore-envelope validation. It embeds the real memory store so it
// satisfies the full metadata.MetadataStore interface while overriding
// Backup/Restore to no-ops.
type tolerantMemStore struct {
	*memory.MemoryMetadataStore
	restoreErr    error
	restoreCalled bool
}

func newTolerantMemStore() *tolerantMemStore {
	return &tolerantMemStore{MemoryMetadataStore: memory.NewMemoryMetadataStoreWithDefaults()}
}

// Override Backup / Restore so we don't depend on the envelope format.
func (t *tolerantMemStore) Backup(ctx context.Context, w io.Writer) (backup.PayloadIDSet, error) {
	return backup.NewPayloadIDSet(), nil
}

func (t *tolerantMemStore) Restore(ctx context.Context, r io.Reader) error {
	t.restoreCalled = true
	if t.restoreErr != nil {
		return t.restoreErr
	}
	// Drain the reader so Close triggers SHA verify in Phase 3 semantics.
	_, _ = io.Copy(io.Discard, r)
	return nil
}

// Confirm interface satisfaction at compile time.
var _ metadata.MetadataStore = (*tolerantMemStore)(nil)
var _ backup.Backupable = (*tolerantMemStore)(nil)

// --- tests ---------------------------------------------------------------

// TestRunRestore_HappyPath: valid manifest, successful restore into a
// fresh engine, swap succeeds, boot verifier bumped exactly once, job
// status=succeeded.
func TestRunRestore_HappyPath(t *testing.T) {
	js := newFakeJobStore()
	fresh := newTolerantMemStore()
	ss := &fakeStores{
		openFn: func(ctx context.Context, cfg *models.MetadataStoreConfig, pathOverride string) (metadata.MetadataStore, error) {
			return fresh, nil
		},
	}
	d := &fakeDest{
		getManifestFn: func(ctx context.Context, id string) (*manifest.Manifest, error) {
			return validManifest(), nil
		},
		getBackupFn: func(ctx context.Context, id string) (*manifest.Manifest, io.ReadCloser, error) {
			return validManifest(), &programmableReader{data: []byte("payload")}, nil
		},
	}

	bumpCalls := 0
	e := New(js, fixedClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	err := e.RunRestore(context.Background(), buildParams(d, ss, &bumpCalls))

	require.NoError(t, err)
	require.True(t, fresh.restoreCalled, "Backupable.Restore must be called on the fresh engine")
	require.Equal(t, 1, ss.swapCount(), "SwapMetadataStore must be called exactly once on success")
	require.Equal(t, 1, bumpCalls, "BumpBootVerifier must be called exactly once on success")
	require.Equal(t, models.BackupStatusSucceeded, js.finalStatus())
	require.Len(t, js.createdJobs, 1)
	require.Equal(t, models.BackupJobKindRestore, js.createdJobs[0].Kind)
}

// TestRunRestore_StoreIDMismatch: manifest.store_id != target →
// ErrStoreIDMismatch, job failed, no fresh engine opened.
func TestRunRestore_StoreIDMismatch(t *testing.T) {
	js, ss, d := happyPathFakes()
	m := validManifest()
	m.StoreID = "some-other-store"
	d.getManifestFn = func(ctx context.Context, id string) (*manifest.Manifest, error) {
		return m, nil
	}

	e := New(js, nil)
	err := e.RunRestore(context.Background(), buildParams(d, ss, nil))

	require.Error(t, err)
	require.True(t, errors.Is(err, ErrStoreIDMismatch), "expected ErrStoreIDMismatch, got %v", err)
	require.Equal(t, 0, ss.openCount(), "fresh engine must NOT be opened on pre-flight failure")
	require.Equal(t, 0, ss.swapCount(), "swap must NOT be called on pre-flight failure")
	require.Equal(t, models.BackupStatusFailed, js.finalStatus())
}

// TestRunRestore_StoreKindMismatch: manifest.store_kind != target →
// ErrStoreKindMismatch, job failed.
func TestRunRestore_StoreKindMismatch(t *testing.T) {
	js, ss, d := happyPathFakes()
	m := validManifest()
	m.StoreKind = "badger"
	d.getManifestFn = func(ctx context.Context, id string) (*manifest.Manifest, error) {
		return m, nil
	}

	e := New(js, nil)
	err := e.RunRestore(context.Background(), buildParams(d, ss, nil))

	require.Error(t, err)
	require.True(t, errors.Is(err, ErrStoreKindMismatch), "expected ErrStoreKindMismatch, got %v", err)
	require.Equal(t, 0, ss.openCount(), "fresh engine must NOT be opened on kind mismatch")
	require.Equal(t, 0, ss.swapCount())
	require.Equal(t, models.BackupStatusFailed, js.finalStatus())
}

// TestRunRestore_ManifestVersionUnsupported: manifest v2 →
// ErrManifestVersionUnsupported, job failed.
func TestRunRestore_ManifestVersionUnsupported(t *testing.T) {
	js, ss, d := happyPathFakes()
	m := validManifest()
	m.ManifestVersion = manifest.CurrentVersion + 1
	d.getManifestFn = func(ctx context.Context, id string) (*manifest.Manifest, error) {
		return m, nil
	}

	e := New(js, nil)
	err := e.RunRestore(context.Background(), buildParams(d, ss, nil))

	require.Error(t, err)
	require.True(t, errors.Is(err, ErrManifestVersionUnsupported),
		"expected ErrManifestVersionUnsupported, got %v", err)
	require.Equal(t, 0, ss.swapCount())
	require.Equal(t, models.BackupStatusFailed, js.finalStatus())
}

// TestRunRestore_EmptyManifestSHA256: manifest SHA-256 empty → error,
// job failed.
func TestRunRestore_EmptyManifestSHA256(t *testing.T) {
	js, ss, d := happyPathFakes()
	m := validManifest()
	m.SHA256 = ""
	d.getManifestFn = func(ctx context.Context, id string) (*manifest.Manifest, error) {
		return m, nil
	}

	e := New(js, nil)
	err := e.RunRestore(context.Background(), buildParams(d, ss, nil))

	require.Error(t, err)
	require.Equal(t, 0, ss.swapCount(), "swap must NOT be called when SHA-256 is empty")
	require.Equal(t, models.BackupStatusFailed, js.finalStatus())
}

// TestRunRestore_SHA256Mismatch: reader.Close returns
// ErrSHA256Mismatch → RunRestore returns wrapped error, job failed.
// Temp engine cleanup path is exercised (fresh engine's Close is
// called via the defer).
func TestRunRestore_SHA256Mismatch(t *testing.T) {
	js := newFakeJobStore()
	fresh := newTolerantMemStore()
	ss := &fakeStores{
		openFn: func(ctx context.Context, cfg *models.MetadataStoreConfig, pathOverride string) (metadata.MetadataStore, error) {
			return fresh, nil
		},
	}
	d := &fakeDest{
		getManifestFn: func(ctx context.Context, id string) (*manifest.Manifest, error) {
			return validManifest(), nil
		},
		getBackupFn: func(ctx context.Context, id string) (*manifest.Manifest, io.ReadCloser, error) {
			return validManifest(), &programmableReader{
				data:     []byte("payload"),
				closeErr: destination.ErrSHA256Mismatch,
			}, nil
		},
	}

	e := New(js, nil)
	err := e.RunRestore(context.Background(), buildParams(d, ss, nil))

	require.Error(t, err)
	require.True(t, errors.Is(err, destination.ErrSHA256Mismatch),
		"expected ErrSHA256Mismatch, got %v", err)
	require.Equal(t, 0, ss.swapCount(), "swap must NOT be called on SHA-256 mismatch")
	require.Equal(t, models.BackupStatusFailed, js.finalStatus())
}

// TestRunRestore_CtxCanceled: cancel ctx mid-Restore; RunRestore
// transitions job to interrupted (NOT failed). Exercises D-17.
func TestRunRestore_CtxCanceled(t *testing.T) {
	js := newFakeJobStore()
	fresh := newTolerantMemStore()
	// Program the tolerant store to return context.Canceled from
	// Restore — simulating an engine that observed ctx cancellation.
	fresh.restoreErr = context.Canceled

	ss := &fakeStores{
		openFn: func(ctx context.Context, cfg *models.MetadataStoreConfig, pathOverride string) (metadata.MetadataStore, error) {
			return fresh, nil
		},
	}
	d := &fakeDest{
		getManifestFn: func(ctx context.Context, id string) (*manifest.Manifest, error) {
			return validManifest(), nil
		},
		getBackupFn: func(ctx context.Context, id string) (*manifest.Manifest, io.ReadCloser, error) {
			return validManifest(), &programmableReader{data: []byte("payload")}, nil
		},
	}

	e := New(js, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel; engine returns ctx.Err() via restoreErr
	err := e.RunRestore(ctx, buildParams(d, ss, nil))

	require.Error(t, err)
	require.True(t, errors.Is(err, context.Canceled),
		"expected context.Canceled in error chain, got %v", err)
	require.Equal(t, 0, ss.swapCount())
	require.Equal(t, models.BackupStatusInterrupted, js.finalStatus(),
		"ctx.Canceled → interrupted (D-17), NOT failed")
}

// TestRunRestore_PostSwapCleanupError: the stores.Service returns an
// old engine whose Close returns an error, but since the swap has
// already committed, RunRestore must return nil. Job is succeeded.
// For the memory kind, CommitSwap only closes the old store (no
// filesystem rename), so this simulates the close-old failure.
func TestRunRestore_PostSwapCleanupError(t *testing.T) {
	js := newFakeJobStore()
	fresh := newTolerantMemStore()

	// closeErrStore is an io.Closer-implementing MetadataStore that
	// fails on Close. Used as the "old" engine returned by Swap.
	oldBroken := &closeErrMemStore{
		MemoryMetadataStore: memory.NewMemoryMetadataStoreWithDefaults(),
		closeErr:            errors.New("simulated close failure"),
	}

	ss := &fakeStores{
		openFn: func(ctx context.Context, cfg *models.MetadataStoreConfig, pathOverride string) (metadata.MetadataStore, error) {
			return fresh, nil
		},
		swapFn: func(name string, newStore metadata.MetadataStore) (metadata.MetadataStore, error) {
			return oldBroken, nil
		},
	}
	d := &fakeDest{
		getManifestFn: func(ctx context.Context, id string) (*manifest.Manifest, error) {
			return validManifest(), nil
		},
		getBackupFn: func(ctx context.Context, id string) (*manifest.Manifest, io.ReadCloser, error) {
			return validManifest(), &programmableReader{data: []byte("payload")}, nil
		},
	}

	bumpCalls := 0
	e := New(js, nil)
	err := e.RunRestore(context.Background(), buildParams(d, ss, &bumpCalls))

	// Post-swap errors are logged, NOT returned. Restore is a success.
	require.NoError(t, err)
	require.Equal(t, 1, ss.swapCount())
	require.Equal(t, 1, bumpCalls, "BumpBootVerifier must still fire on post-swap cleanup errors")
	require.Equal(t, models.BackupStatusSucceeded, js.finalStatus(),
		"post-swap cleanup errors do NOT alter job status (still succeeded)")
}

// TestRunRestore_BumpBootVerifierCalled: a non-nil BumpBootVerifier is
// invoked exactly once on a successful restore. Covers D-09 wiring.
func TestRunRestore_BumpBootVerifierCalled(t *testing.T) {
	js := newFakeJobStore()
	fresh := newTolerantMemStore()
	ss := &fakeStores{
		openFn: func(ctx context.Context, cfg *models.MetadataStoreConfig, pathOverride string) (metadata.MetadataStore, error) {
			return fresh, nil
		},
	}
	d := &fakeDest{
		getManifestFn: func(ctx context.Context, id string) (*manifest.Manifest, error) {
			return validManifest(), nil
		},
		getBackupFn: func(ctx context.Context, id string) (*manifest.Manifest, io.ReadCloser, error) {
			return validManifest(), &programmableReader{data: []byte("payload")}, nil
		},
	}

	bumpCalls := 0
	e := New(js, nil)
	require.NoError(t, e.RunRestore(context.Background(), buildParams(d, ss, &bumpCalls)))
	require.Equal(t, 1, bumpCalls)

	// Re-run with a NIL BumpBootVerifier — must not panic.
	p := buildParams(d, ss, nil)
	p.BumpBootVerifier = nil
	require.NoError(t, e.RunRestore(context.Background(), p))
}

// closeErrMemStore is a real memory store whose Close returns a
// programmed error. Used by TestRunRestore_PostSwapCleanupError to
// simulate a displaced-old-engine close failure.
type closeErrMemStore struct {
	*memory.MemoryMetadataStore
	closeErr error
}

func (c *closeErrMemStore) Close() error {
	return c.closeErr
}

// Satisfies metadata.MetadataStore (via embedded real memory store).
var _ metadata.MetadataStore = (*closeErrMemStore)(nil)
