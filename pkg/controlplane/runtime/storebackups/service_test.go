package storebackups

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/backup"
	"github.com/marmos91/dittofs/pkg/backup/destination"
	"github.com/marmos91/dittofs/pkg/backup/manifest"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	cpstore "github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// ---- Test fakes ----

// stubResolver resolves every (kind, id) pair to the same configured source+identity.
// Used in happy-path Service tests that don't need runtime-registry plumbing.
type stubResolver struct {
	src       backup.Backupable
	storeID   string
	storeKind string
	err       error
}

func (r *stubResolver) Resolve(ctx context.Context, kind, id string) (backup.Backupable, string, string, error) {
	if r.err != nil {
		return nil, "", "", r.err
	}
	return r.src, r.storeID, r.storeKind, nil
}

// controlledDestination exposes Put/Delete hooks for timing + ordering tests.
type controlledDestination struct {
	mu           sync.Mutex
	putCalls     int32
	deleteCalls  int32
	onPut        func(ctx context.Context) error
	putBlockCh   chan struct{} // if non-nil, PutBackup blocks on ctx.Done OR this channel
	deleteOrder  []string      // record IDs seen in Delete, in order
	putStamp     int64
	deleteStamps []int64 // appended in order
}

func (d *controlledDestination) PutBackup(ctx context.Context, m *manifest.Manifest, payload io.Reader) error {
	atomic.AddInt32(&d.putCalls, 1)
	d.mu.Lock()
	d.putStamp = time.Now().UnixNano()
	onPut := d.onPut
	blockCh := d.putBlockCh
	d.mu.Unlock()

	// Drain the payload so the source goroutine can close. Without this, the
	// source.Backup would block writing into a full pipe.
	if payload != nil {
		_, _ = io.Copy(io.Discard, payload)
	}

	if onPut != nil {
		if err := onPut(ctx); err != nil {
			return err
		}
	}
	if blockCh != nil {
		select {
		case <-blockCh:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	// Stamp size + sha so the Manifest validates.
	m.SizeBytes = 1
	m.SHA256 = "deadbeef"
	return nil
}

func (d *controlledDestination) GetBackup(ctx context.Context, id string) (*manifest.Manifest, io.ReadCloser, error) {
	return nil, nil, errors.New("not implemented")
}

func (d *controlledDestination) List(ctx context.Context) ([]destination.BackupDescriptor, error) {
	return nil, nil
}

func (d *controlledDestination) Stat(ctx context.Context, id string) (*destination.BackupDescriptor, error) {
	return nil, nil
}

func (d *controlledDestination) Delete(ctx context.Context, id string) error {
	atomic.AddInt32(&d.deleteCalls, 1)
	d.mu.Lock()
	d.deleteOrder = append(d.deleteOrder, id)
	d.deleteStamps = append(d.deleteStamps, time.Now().UnixNano())
	d.mu.Unlock()
	return nil
}

func (d *controlledDestination) ValidateConfig(ctx context.Context) error { return nil }
func (d *controlledDestination) Close() error                             { return nil }

var _ destination.Destination = (*controlledDestination)(nil)

// ---- Test harness ----

func newTestStore(t *testing.T) cpstore.Store {
	t.Helper()
	s, err := cpstore.New(&cpstore.Config{
		Type:   cpstore.DatabaseTypeSQLite,
		SQLite: cpstore.SQLiteConfig{Path: ":memory:"},
	})
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	return s
}

func seedRepo(t *testing.T, s cpstore.Store, opts ...func(*models.BackupRepo)) *models.BackupRepo {
	t.Helper()
	ctx := context.Background()
	sched := "*/5 * * * *"
	repo := &models.BackupRepo{
		TargetID:   "cfg-meta",
		TargetKind: "metadata",
		Name:       "default",
		Kind:       models.BackupRepoKindLocal,
		Schedule:   &sched,
	}
	for _, fn := range opts {
		fn(repo)
	}
	id, err := s.CreateBackupRepo(ctx, repo)
	if err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	repo.ID = id
	return repo
}

// newServiceWithStubs constructs a Service with a stub resolver + controlled destination.
func newServiceWithStubs(t *testing.T, s cpstore.Store, src backup.Backupable, dst destination.Destination) *Service {
	t.Helper()
	resolver := &stubResolver{src: src, storeID: "cfg-meta", storeKind: "memory"}
	factory := func(ctx context.Context, repo *models.BackupRepo) (destination.Destination, error) {
		return dst, nil
	}
	svc := New(s, resolver, 500*time.Millisecond, WithDestinationFactory(factory))
	t.Cleanup(func() { _ = svc.Stop(context.Background()) })
	return svc
}

// ---- Tests ----

// T1 New: constructor returns non-nil with default internals.
func TestService_New(t *testing.T) {
	s := newTestStore(t)
	svc := New(s, &stubResolver{}, 0)
	if svc == nil {
		t.Fatal("New returned nil")
	}
	if svc.shutdownTimeout != DefaultShutdownTimeout {
		t.Errorf("expected default shutdown timeout, got %v", svc.shutdownTimeout)
	}
	if svc.sched == nil {
		t.Error("scheduler should be initialized")
	}
	if svc.overlap == nil {
		t.Error("overlap guard should be initialized")
	}
	if svc.exec == nil {
		t.Error("executor should be initialized")
	}
}

// T2 Serve recovers interrupted jobs (D-19 / SAFETY-02).
func TestService_ServeRecoversInterruptedJobs(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Seed a "running" job that simulates a pre-restart orphan.
	startedAt := time.Now().Add(-10 * time.Minute)
	_, err := s.CreateBackupJob(ctx, &models.BackupJob{
		Kind:      models.BackupJobKindBackup,
		RepoID:    "orphan-repo",
		Status:    models.BackupStatusRunning,
		StartedAt: &startedAt,
	})
	if err != nil {
		t.Fatalf("seed orphan job: %v", err)
	}

	svc := New(s, &stubResolver{}, 100*time.Millisecond)
	t.Cleanup(func() { _ = svc.Stop(context.Background()) })

	if err := svc.Serve(ctx); err != nil {
		t.Fatalf("Serve failed: %v", err)
	}

	// Orphan job should now be interrupted.
	jobs, err := s.ListBackupJobs(ctx, models.BackupJobKindBackup, models.BackupStatusInterrupted)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Errorf("expected 1 interrupted job, got %d", len(jobs))
	}
}

// T3 Serve loads schedules for repos with non-empty cron.
func TestService_ServeLoadsSchedules(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	repo := seedRepo(t, s)

	svc := New(s, &stubResolver{}, 100*time.Millisecond)
	t.Cleanup(func() { _ = svc.Stop(context.Background()) })

	if err := svc.Serve(ctx); err != nil {
		t.Fatalf("Serve failed: %v", err)
	}

	if !svc.sched.IsRegistered(repo.ID) {
		t.Errorf("repo %s should be registered with scheduler", repo.ID)
	}
}

// T4 Serve skips repos with invalid schedules (D-06).
func TestService_ServeSkipsInvalidSchedules(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Seed one valid + one invalid.
	good := seedRepo(t, s)
	bad := seedRepo(t, s, func(r *models.BackupRepo) {
		r.Name = "bad-repo"
		badSched := "not-a-cron-expr"
		r.Schedule = &badSched
	})

	svc := New(s, &stubResolver{}, 100*time.Millisecond)
	t.Cleanup(func() { _ = svc.Stop(context.Background()) })

	if err := svc.Serve(ctx); err != nil {
		t.Fatalf("Serve should not fail on invalid cron: %v", err)
	}
	if !svc.sched.IsRegistered(good.ID) {
		t.Error("good repo should be registered")
	}
	if svc.sched.IsRegistered(bad.ID) {
		t.Error("bad repo should be skipped (D-06)")
	}
}

// T5 RegisterRepo loads repo from store and installs scheduler entry.
func TestService_RegisterRepo(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	repo := seedRepo(t, s)

	svc := New(s, &stubResolver{}, 100*time.Millisecond)
	t.Cleanup(func() { _ = svc.Stop(context.Background()) })

	if err := svc.RegisterRepo(ctx, repo.ID); err != nil {
		t.Fatalf("RegisterRepo failed: %v", err)
	}
	if !svc.sched.IsRegistered(repo.ID) {
		t.Error("repo should be registered after RegisterRepo")
	}

	// Unknown repo returns ErrRepoNotFound.
	err := svc.RegisterRepo(ctx, "no-such-repo")
	if err == nil || !errors.Is(err, ErrRepoNotFound) {
		t.Errorf("expected ErrRepoNotFound for missing repo, got %v", err)
	}
}

// T6 UnregisterRepo removes scheduler entry; no-op if not registered.
func TestService_UnregisterRepo(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	repo := seedRepo(t, s)

	svc := New(s, &stubResolver{}, 100*time.Millisecond)
	t.Cleanup(func() { _ = svc.Stop(context.Background()) })

	_ = svc.RegisterRepo(ctx, repo.ID)
	if err := svc.UnregisterRepo(ctx, repo.ID); err != nil {
		t.Fatalf("UnregisterRepo failed: %v", err)
	}
	if svc.sched.IsRegistered(repo.ID) {
		t.Error("repo should not be registered after UnregisterRepo")
	}
	// No-op for unknown id.
	if err := svc.UnregisterRepo(ctx, "no-such-repo"); err != nil {
		t.Errorf("UnregisterRepo on unknown id should not error, got %v", err)
	}
}

// T7 UpdateRepo = Unregister + Register.
func TestService_UpdateRepo(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	repo := seedRepo(t, s)

	svc := New(s, &stubResolver{}, 100*time.Millisecond)
	t.Cleanup(func() { _ = svc.Stop(context.Background()) })

	if err := svc.RegisterRepo(ctx, repo.ID); err != nil {
		t.Fatalf("initial RegisterRepo: %v", err)
	}

	// Update schedule.
	newSched := "0 * * * *"
	repo.Schedule = &newSched
	if err := s.UpdateBackupRepo(ctx, repo); err != nil {
		t.Fatalf("store update: %v", err)
	}

	if err := svc.UpdateRepo(ctx, repo.ID); err != nil {
		t.Fatalf("UpdateRepo failed: %v", err)
	}
	if !svc.sched.IsRegistered(repo.ID) {
		t.Error("repo should remain registered after UpdateRepo")
	}
}

// T8 RunBackup happy path.
func TestService_RunBackup_Happy(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	repo := seedRepo(t, s)

	src := memory.NewMemoryMetadataStoreWithDefaults()
	dst := &controlledDestination{}
	svc := newServiceWithStubs(t, s, src, dst)

	rec, err := svc.RunBackup(ctx, repo.ID)
	if err != nil {
		t.Fatalf("RunBackup failed: %v", err)
	}
	if rec == nil {
		t.Fatal("expected non-nil record")
	}
	if rec.Status != models.BackupStatusSucceeded {
		t.Errorf("record status = %q, want succeeded", rec.Status)
	}
	if atomic.LoadInt32(&dst.putCalls) != 1 {
		t.Errorf("PutBackup calls = %d, want 1", dst.putCalls)
	}
}

// T9 RunBackup mutex contention — second caller gets ErrBackupAlreadyRunning.
func TestService_RunBackup_MutexContention(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	repo := seedRepo(t, s)

	src := memory.NewMemoryMetadataStoreWithDefaults()
	// First Put blocks until we signal; second RunBackup must see the mutex held.
	blockCh := make(chan struct{})
	dst := &controlledDestination{putBlockCh: blockCh}
	svc := newServiceWithStubs(t, s, src, dst)

	var (
		wg         sync.WaitGroup
		successCnt atomic.Int32
		busyCnt    atomic.Int32
		otherCnt   atomic.Int32
	)

	launch := func() {
		defer wg.Done()
		_, err := svc.RunBackup(ctx, repo.ID)
		switch {
		case err == nil:
			successCnt.Add(1)
		case errors.Is(err, ErrBackupAlreadyRunning):
			busyCnt.Add(1)
		default:
			otherCnt.Add(1)
			t.Logf("unexpected error: %v", err)
		}
	}

	wg.Add(1)
	go launch()

	// Wait until the first caller has acquired the mutex and entered PutBackup.
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&dst.putCalls) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if atomic.LoadInt32(&dst.putCalls) == 0 {
		close(blockCh)
		wg.Wait()
		t.Fatal("first RunBackup never reached PutBackup")
	}

	// Launch second caller — should bounce off the mutex immediately.
	wg.Add(1)
	go launch()

	// Give the second goroutine a moment to try and fail.
	time.Sleep(50 * time.Millisecond)

	// Release the first caller.
	close(blockCh)
	wg.Wait()

	if got, want := successCnt.Load(), int32(1); got != want {
		t.Errorf("success count = %d, want %d", got, want)
	}
	if got, want := busyCnt.Load(), int32(1); got != want {
		t.Errorf("ErrBackupAlreadyRunning count = %d, want %d", got, want)
	}
	if otherCnt.Load() != 0 {
		t.Errorf("unexpected other errors: %d", otherCnt.Load())
	}
}

// T10 RunBackup sequence: PutBackup precedes any Destination.Delete calls
// from retention — the mutex covers the full pipeline (D-08, SCHED-06).
func TestService_RunBackup_SequencePutBeforeDelete(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Configure tight keep-count so that after the 2nd backup the 1st is pruned.
	keep := 1
	repo := seedRepo(t, s, func(r *models.BackupRepo) {
		r.KeepCount = &keep
	})

	src := memory.NewMemoryMetadataStoreWithDefaults()
	dst := &controlledDestination{}
	svc := newServiceWithStubs(t, s, src, dst)

	// First backup populates the record store; no retention candidates yet.
	if _, err := svc.RunBackup(ctx, repo.ID); err != nil {
		t.Fatalf("first RunBackup: %v", err)
	}
	// Second backup triggers retention — retention.Delete should run after PutBackup.
	if _, err := svc.RunBackup(ctx, repo.ID); err != nil {
		t.Fatalf("second RunBackup: %v", err)
	}

	dst.mu.Lock()
	putStamp := dst.putStamp
	stamps := append([]int64(nil), dst.deleteStamps...)
	dst.mu.Unlock()

	if putStamp == 0 {
		t.Fatal("PutBackup was never called")
	}
	if len(stamps) == 0 {
		t.Fatal("Destination.Delete was never called — retention didn't prune")
	}
	for _, ds := range stamps {
		if ds < putStamp {
			t.Errorf("Destination.Delete (stamp=%d) preceded latest PutBackup (stamp=%d)", ds, putStamp)
		}
	}
}

// T11 RunBackup resolves target — errors surface from resolver.
func TestService_RunBackup_ResolverErrors(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	repo := seedRepo(t, s)

	t.Run("unknown_kind", func(t *testing.T) {
		resolver := &stubResolver{err: errors.New("bad kind: " + ErrInvalidTargetKind.Error())}
		// Wrap the sentinel so errors.Is works.
		resolver.err = wrapWithSentinel(ErrInvalidTargetKind, "bad kind")
		dst := &controlledDestination{}
		factory := func(ctx context.Context, _ *models.BackupRepo) (destination.Destination, error) {
			return dst, nil
		}
		svc := New(s, resolver, 100*time.Millisecond, WithDestinationFactory(factory))
		t.Cleanup(func() { _ = svc.Stop(context.Background()) })

		_, err := svc.RunBackup(ctx, repo.ID)
		if err == nil {
			t.Fatal("expected error")
		}
		if !errors.Is(err, ErrInvalidTargetKind) {
			t.Errorf("expected ErrInvalidTargetKind-wrapped, got %v", err)
		}
	})

	t.Run("missing_config", func(t *testing.T) {
		resolver := &stubResolver{err: wrapWithSentinel(ErrRepoNotFound, "missing target config")}
		dst := &controlledDestination{}
		factory := func(ctx context.Context, _ *models.BackupRepo) (destination.Destination, error) {
			return dst, nil
		}
		svc := New(s, resolver, 100*time.Millisecond, WithDestinationFactory(factory))
		t.Cleanup(func() { _ = svc.Stop(context.Background()) })

		_, err := svc.RunBackup(ctx, repo.ID)
		if err == nil {
			t.Fatal("expected error")
		}
		if !errors.Is(err, ErrRepoNotFound) {
			t.Errorf("expected ErrRepoNotFound-wrapped, got %v", err)
		}
	})
}

// wrapWithSentinel constructs an error that wraps `sentinel` using %w.
func wrapWithSentinel(sentinel error, msg string) error {
	return &wrappedErr{sentinel: sentinel, msg: msg}
}

type wrappedErr struct {
	sentinel error
	msg      string
}

func (w *wrappedErr) Error() string { return w.msg + ": " + w.sentinel.Error() }
func (w *wrappedErr) Unwrap() error { return w.sentinel }

// T12 Stop cancels in-flight RunBackup within 500ms (D-18).
func TestService_Stop_CancelsInFlight(t *testing.T) {
	s := newTestStore(t)
	repo := seedRepo(t, s)

	src := memory.NewMemoryMetadataStoreWithDefaults()
	blockCh := make(chan struct{})
	defer close(blockCh) // release even if Stop path fails to cancel
	dst := &controlledDestination{putBlockCh: blockCh}
	svc := newServiceWithStubs(t, s, src, dst)

	// Serve to attach a serveCtx.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := svc.Serve(ctx); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	// Launch RunBackup — it will block inside PutBackup.
	type runResult struct {
		rec *models.BackupRecord
		err error
	}
	resultCh := make(chan runResult, 1)
	go func() {
		rec, err := svc.RunBackup(ctx, repo.ID)
		resultCh <- runResult{rec: rec, err: err}
	}()

	// Wait until the PutBackup goroutine has started.
	deadline := time.Now().Add(time.Second)
	for atomic.LoadInt32(&dst.putCalls) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if atomic.LoadInt32(&dst.putCalls) == 0 {
		t.Fatal("PutBackup never started")
	}

	stopStart := time.Now()
	if err := svc.Stop(context.Background()); err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}

	select {
	case res := <-resultCh:
		elapsed := time.Since(stopStart)
		if elapsed > 500*time.Millisecond {
			t.Errorf("RunBackup didn't cancel within 500ms: took %v", elapsed)
		}
		if res.err == nil {
			t.Error("RunBackup should have returned an error after Stop")
		}
	case <-time.After(time.Second):
		t.Fatal("RunBackup didn't return within 1s of Stop")
	}
}

// T13 Runtime delegation — exercised via svc.ValidateSchedule (matches scheduler validation).
func TestService_ValidateSchedule(t *testing.T) {
	s := newTestStore(t)
	svc := New(s, &stubResolver{}, 100*time.Millisecond)
	t.Cleanup(func() { _ = svc.Stop(context.Background()) })

	if err := svc.ValidateSchedule("*/5 * * * *"); err != nil {
		t.Errorf("valid schedule should pass: %v", err)
	}
	err := svc.ValidateSchedule("not-a-cron")
	if err == nil {
		t.Fatal("expected error for invalid schedule")
	}
	if !errors.Is(err, ErrScheduleInvalid) {
		t.Errorf("expected ErrScheduleInvalid-wrapped, got %v", err)
	}
}

// T14 On-demand and scheduled paths share the same mutex (D-23). Scheduled tick
// is simulated by manually calling runScheduledBackup while an on-demand call
// holds the mutex — the scheduled path should get ErrBackupAlreadyRunning (which
// the scheduler then logs-and-skips per D-01 policy).
func TestService_ScheduledAndOnDemandShareMutex(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	repo := seedRepo(t, s)

	src := memory.NewMemoryMetadataStoreWithDefaults()
	blockCh := make(chan struct{})
	dst := &controlledDestination{putBlockCh: blockCh}
	svc := newServiceWithStubs(t, s, src, dst)

	var wg sync.WaitGroup
	var onDemandErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, onDemandErr = svc.RunBackup(ctx, repo.ID)
	}()

	// Wait until on-demand has entered PutBackup.
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&dst.putCalls) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if atomic.LoadInt32(&dst.putCalls) == 0 {
		close(blockCh)
		wg.Wait()
		t.Fatal("on-demand RunBackup never reached PutBackup")
	}

	// Simulate scheduled tick via the internal JobFn path.
	schedErr := svc.runScheduledBackup(ctx, repo.ID)
	if schedErr == nil || !errors.Is(schedErr, ErrBackupAlreadyRunning) {
		t.Errorf("scheduled tick should see ErrBackupAlreadyRunning, got %v", schedErr)
	}

	close(blockCh)
	wg.Wait()
	if onDemandErr != nil {
		t.Errorf("on-demand RunBackup failed: %v", onDemandErr)
	}
}
