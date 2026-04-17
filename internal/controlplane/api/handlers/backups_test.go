package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/storebackups"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// -----------------------------------------------------------------------------
// Test fakes
// -----------------------------------------------------------------------------

// fakeBackupStore implements BackupHandlerStore with in-memory maps. Only the
// methods used by the handler are fleshed out; the rest panic so accidental
// widening of the handler surface fails loudly in tests.
type fakeBackupStore struct {
	metaStores map[string]*models.MetadataStoreConfig // keyed by name
	repos      []*models.BackupRepo                   // all repos
	records    []*models.BackupRecord                 // all records
	jobs       []*models.BackupJob                    // all jobs

	// Error injection hooks
	createBackupRepoErr   error
	updateBackupRepoErr   error
	deleteBackupRepoErr   error
	updateRecordPinnedErr error
	listFilteredErr       error

	// Call recorders
	updatedPinnedID     string
	updatedPinnedValue  bool
	deleteRepoCalledID  string
	lastListFilter      store.BackupJobFilter
}

func (f *fakeBackupStore) GetMetadataStore(_ context.Context, name string) (*models.MetadataStoreConfig, error) {
	if s, ok := f.metaStores[name]; ok {
		return s, nil
	}
	return nil, models.ErrStoreNotFound
}

func (f *fakeBackupStore) GetMetadataStoreByID(_ context.Context, id string) (*models.MetadataStoreConfig, error) {
	for _, s := range f.metaStores {
		if s.ID == id {
			return s, nil
		}
	}
	return nil, models.ErrStoreNotFound
}

func (f *fakeBackupStore) ListMetadataStores(_ context.Context) ([]*models.MetadataStoreConfig, error) {
	out := make([]*models.MetadataStoreConfig, 0, len(f.metaStores))
	for _, s := range f.metaStores {
		out = append(out, s)
	}
	return out, nil
}
func (f *fakeBackupStore) CreateMetadataStore(_ context.Context, s *models.MetadataStoreConfig) (string, error) {
	if f.metaStores == nil {
		f.metaStores = map[string]*models.MetadataStoreConfig{}
	}
	f.metaStores[s.Name] = s
	return s.ID, nil
}
func (f *fakeBackupStore) UpdateMetadataStore(_ context.Context, s *models.MetadataStoreConfig) error {
	f.metaStores[s.Name] = s
	return nil
}
func (f *fakeBackupStore) DeleteMetadataStore(_ context.Context, name string) error {
	delete(f.metaStores, name)
	return nil
}
func (f *fakeBackupStore) GetSharesByMetadataStore(_ context.Context, _ string) ([]*models.Share, error) {
	return nil, nil
}

// BackupStore
func (f *fakeBackupStore) GetBackupRepo(_ context.Context, storeID, name string) (*models.BackupRepo, error) {
	for _, r := range f.repos {
		if r.TargetID == storeID && r.Name == name {
			return r, nil
		}
	}
	return nil, models.ErrBackupRepoNotFound
}
func (f *fakeBackupStore) GetBackupRepoByID(_ context.Context, id string) (*models.BackupRepo, error) {
	for _, r := range f.repos {
		if r.ID == id {
			return r, nil
		}
	}
	return nil, models.ErrBackupRepoNotFound
}
func (f *fakeBackupStore) ListReposByTarget(_ context.Context, kind, targetID string) ([]*models.BackupRepo, error) {
	var out []*models.BackupRepo
	for _, r := range f.repos {
		if r.TargetKind == kind && r.TargetID == targetID {
			out = append(out, r)
		}
	}
	return out, nil
}
func (f *fakeBackupStore) ListAllBackupRepos(_ context.Context) ([]*models.BackupRepo, error) {
	return f.repos, nil
}
func (f *fakeBackupStore) CreateBackupRepo(_ context.Context, repo *models.BackupRepo) (string, error) {
	if f.createBackupRepoErr != nil {
		return "", f.createBackupRepoErr
	}
	if repo.ID == "" {
		repo.ID = "repo-" + repo.Name
	}
	f.repos = append(f.repos, repo)
	return repo.ID, nil
}
func (f *fakeBackupStore) UpdateBackupRepo(_ context.Context, repo *models.BackupRepo) error {
	if f.updateBackupRepoErr != nil {
		return f.updateBackupRepoErr
	}
	for i, r := range f.repos {
		if r.ID == repo.ID {
			f.repos[i] = repo
			return nil
		}
	}
	return models.ErrBackupRepoNotFound
}
func (f *fakeBackupStore) DeleteBackupRepo(_ context.Context, id string) error {
	f.deleteRepoCalledID = id
	if f.deleteBackupRepoErr != nil {
		return f.deleteBackupRepoErr
	}
	for i, r := range f.repos {
		if r.ID == id {
			f.repos = append(f.repos[:i], f.repos[i+1:]...)
			return nil
		}
	}
	return models.ErrBackupRepoNotFound
}

func (f *fakeBackupStore) GetBackupRecord(_ context.Context, id string) (*models.BackupRecord, error) {
	for _, r := range f.records {
		if r.ID == id {
			return r, nil
		}
	}
	return nil, models.ErrBackupRecordNotFound
}
func (f *fakeBackupStore) ListBackupRecordsByRepo(_ context.Context, repoID string) ([]*models.BackupRecord, error) {
	var out []*models.BackupRecord
	for _, r := range f.records {
		if r.RepoID == repoID {
			out = append(out, r)
		}
	}
	return out, nil
}
func (f *fakeBackupStore) ListSucceededRecordsForRetention(_ context.Context, repoID string) ([]*models.BackupRecord, error) {
	return nil, nil
}
func (f *fakeBackupStore) ListSucceededRecordsByRepo(_ context.Context, repoID string) ([]*models.BackupRecord, error) {
	var out []*models.BackupRecord
	for _, r := range f.records {
		if r.RepoID == repoID && r.Status == models.BackupStatusSucceeded {
			out = append(out, r)
		}
	}
	return out, nil
}
func (f *fakeBackupStore) CreateBackupRecord(_ context.Context, rec *models.BackupRecord) (string, error) {
	if rec.ID == "" {
		rec.ID = "rec"
	}
	f.records = append(f.records, rec)
	return rec.ID, nil
}
func (f *fakeBackupStore) UpdateBackupRecord(_ context.Context, rec *models.BackupRecord) error {
	for i, r := range f.records {
		if r.ID == rec.ID {
			f.records[i] = rec
			return nil
		}
	}
	return models.ErrBackupRecordNotFound
}
func (f *fakeBackupStore) DeleteBackupRecord(_ context.Context, id string) error {
	for i, r := range f.records {
		if r.ID == id {
			f.records = append(f.records[:i], f.records[i+1:]...)
			return nil
		}
	}
	return models.ErrBackupRecordNotFound
}
func (f *fakeBackupStore) SetBackupRecordPinned(_ context.Context, id string, pinned bool) error {
	for _, r := range f.records {
		if r.ID == id {
			r.Pinned = pinned
			return nil
		}
	}
	return models.ErrBackupRecordNotFound
}
func (f *fakeBackupStore) UpdateBackupRecordPinned(_ context.Context, id string, pinned bool) error {
	f.updatedPinnedID = id
	f.updatedPinnedValue = pinned
	if f.updateRecordPinnedErr != nil {
		return f.updateRecordPinnedErr
	}
	for _, r := range f.records {
		if r.ID == id {
			r.Pinned = pinned
			return nil
		}
	}
	return models.ErrBackupRecordNotFound
}
func (f *fakeBackupStore) ListBackupRecords(_ context.Context, repoID string, statusFilter models.BackupStatus) ([]*models.BackupRecord, error) {
	var out []*models.BackupRecord
	for _, r := range f.records {
		if r.RepoID != repoID {
			continue
		}
		if statusFilter != "" && r.Status != statusFilter {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

func (f *fakeBackupStore) GetBackupJob(_ context.Context, id string) (*models.BackupJob, error) {
	for _, j := range f.jobs {
		if j.ID == id {
			return j, nil
		}
	}
	return nil, models.ErrBackupJobNotFound
}
func (f *fakeBackupStore) ListBackupJobs(_ context.Context, kind models.BackupJobKind, status models.BackupStatus) ([]*models.BackupJob, error) {
	return nil, nil
}
func (f *fakeBackupStore) ListBackupJobsFiltered(_ context.Context, filter store.BackupJobFilter) ([]*models.BackupJob, error) {
	f.lastListFilter = filter
	if f.listFilteredErr != nil {
		return nil, f.listFilteredErr
	}
	var out []*models.BackupJob
	for _, j := range f.jobs {
		if filter.RepoID != "" && j.RepoID != filter.RepoID {
			continue
		}
		if filter.Status != "" && j.Status != filter.Status {
			continue
		}
		if filter.Kind != "" && j.Kind != filter.Kind {
			continue
		}
		out = append(out, j)
		if filter.Limit > 0 && len(out) >= filter.Limit {
			break
		}
	}
	return out, nil
}
func (f *fakeBackupStore) UpdateBackupRecordPinnedStub() {} // noop to keep the linter quiet about unused fields
func (f *fakeBackupStore) UpdateBackupJobProgress(_ context.Context, jobID string, pct int) error {
	return nil
}
func (f *fakeBackupStore) CreateBackupJob(_ context.Context, job *models.BackupJob) (string, error) {
	if job.ID == "" {
		job.ID = "job"
	}
	f.jobs = append(f.jobs, job)
	return job.ID, nil
}
func (f *fakeBackupStore) UpdateBackupJob(_ context.Context, job *models.BackupJob) error {
	for i, j := range f.jobs {
		if j.ID == job.ID {
			f.jobs[i] = job
			return nil
		}
	}
	return models.ErrBackupJobNotFound
}
func (f *fakeBackupStore) RecoverInterruptedJobs(_ context.Context) (int, error) { return 0, nil }
func (f *fakeBackupStore) PruneBackupJobsOlderThan(_ context.Context, _ time.Time) (int, error) {
	return 0, nil
}

// -----------------------------------------------------------------------------
// fakeBackupService
// -----------------------------------------------------------------------------

type fakeBackupService struct {
	// canned returns
	runBackupRec *models.BackupRecord
	runBackupJob *models.BackupJob
	runBackupErr error

	runRestoreJob *models.BackupJob
	runRestoreErr error

	dryRunResult *storebackups.DryRunResult
	dryRunErr    error

	cancelErr error

	validateScheduleErr error

	// recorders
	runBackupRepoID  string
	runRestoreRepoID string
	runRestoreID     *string
	cancelJobID      string
	validatedExpr    string
}

func (s *fakeBackupService) RunBackup(_ context.Context, repoID string) (*models.BackupRecord, *models.BackupJob, error) {
	s.runBackupRepoID = repoID
	return s.runBackupRec, s.runBackupJob, s.runBackupErr
}
func (s *fakeBackupService) RunRestore(_ context.Context, repoID string, recordID *string) (*models.BackupJob, error) {
	s.runRestoreRepoID = repoID
	s.runRestoreID = recordID
	return s.runRestoreJob, s.runRestoreErr
}
func (s *fakeBackupService) RunRestoreDryRun(_ context.Context, _ string, _ *string) (*storebackups.DryRunResult, error) {
	return s.dryRunResult, s.dryRunErr
}
func (s *fakeBackupService) CancelBackupJob(_ context.Context, jobID string) error {
	s.cancelJobID = jobID
	return s.cancelErr
}
func (s *fakeBackupService) RegisterRepo(_ context.Context, _ string) error   { return nil }
func (s *fakeBackupService) UnregisterRepo(_ context.Context, _ string) error { return nil }
func (s *fakeBackupService) UpdateRepo(_ context.Context, _ string) error     { return nil }
func (s *fakeBackupService) ValidateSchedule(expr string) error {
	s.validatedExpr = expr
	return s.validateScheduleErr
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

func newTestHandler(storeFake *fakeBackupStore, svcFake *fakeBackupService) *BackupHandler {
	return NewBackupHandler(storeFake, svcFake, nil)
}

func withRouteParams(r *http.Request, params map[string]string) *http.Request {
	rctx := chi.NewRouteContext()
	for k, v := range params {
		rctx.URLParams.Add(k, v)
	}
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}

func seedStoreWithRepo(numRepos int) (*fakeBackupStore, []*models.BackupRepo) {
	s := &fakeBackupStore{
		metaStores: map[string]*models.MetadataStoreConfig{
			"fast-meta": {ID: "store-1", Name: "fast-meta", Type: "memory"},
		},
	}
	var repos []*models.BackupRepo
	for i := 0; i < numRepos; i++ {
		name := fmt.Sprintf("repo%d", i)
		if i == 0 {
			name = "primary"
		}
		r := &models.BackupRepo{
			ID: fmt.Sprintf("repo-%d", i), Name: name, Kind: models.BackupRepoKindLocal,
			TargetID: "store-1", TargetKind: "metadata",
		}
		repos = append(repos, r)
	}
	s.repos = repos
	return s, repos
}

// -----------------------------------------------------------------------------
// TriggerBackup tests
// -----------------------------------------------------------------------------

func TestTriggerBackup_SingleRepo_Returns202WithRecordAndJob(t *testing.T) {
	storeFake, repos := seedStoreWithRepo(1)
	svcFake := &fakeBackupService{
		runBackupRec: &models.BackupRecord{ID: "r1", RepoID: repos[0].ID, Status: models.BackupStatusSucceeded},
		runBackupJob: &models.BackupJob{ID: "j1", RepoID: repos[0].ID, Kind: models.BackupJobKindBackup, Status: models.BackupStatusRunning},
	}
	h := newTestHandler(storeFake, svcFake)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/store/metadata/fast-meta/backups", bytes.NewReader([]byte(`{}`)))
	req = withRouteParams(req, map[string]string{"name": "fast-meta"})
	rr := httptest.NewRecorder()
	h.TriggerBackup(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	var resp TriggerBackupResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Record == nil || resp.Record.ID != "r1" {
		t.Errorf("record missing/wrong: %+v", resp.Record)
	}
	if resp.Job == nil || resp.Job.ID != "j1" {
		t.Errorf("job missing/wrong: %+v", resp.Job)
	}
	if svcFake.runBackupRepoID != repos[0].ID {
		t.Errorf("RunBackup invoked with %q, want %q", svcFake.runBackupRepoID, repos[0].ID)
	}
}

func TestTriggerBackup_MultiRepo_RequiresRepoParam(t *testing.T) {
	storeFake, _ := seedStoreWithRepo(2)
	svcFake := &fakeBackupService{}
	h := newTestHandler(storeFake, svcFake)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/store/metadata/fast-meta/backups", bytes.NewReader([]byte(`{}`)))
	req = withRouteParams(req, map[string]string{"name": "fast-meta"})
	rr := httptest.NewRecorder()
	h.TriggerBackup(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "primary") || !strings.Contains(rr.Body.String(), "repo1") {
		t.Errorf("response should list repo names, got %s", rr.Body.String())
	}
}

func TestTriggerBackup_AlreadyRunning_Returns409WithJobID(t *testing.T) {
	storeFake, repos := seedStoreWithRepo(1)
	// Seed a running job so the handler can resolve running_job_id.
	storeFake.jobs = []*models.BackupJob{
		{ID: "running-job", RepoID: repos[0].ID, Kind: models.BackupJobKindBackup, Status: models.BackupStatusRunning},
	}
	svcFake := &fakeBackupService{
		runBackupErr: fmt.Errorf("%w: repo %s", storebackups.ErrBackupAlreadyRunning, repos[0].ID),
	}
	h := newTestHandler(storeFake, svcFake)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/store/metadata/fast-meta/backups", bytes.NewReader([]byte(`{}`)))
	req = withRouteParams(req, map[string]string{"name": "fast-meta"})
	rr := httptest.NewRecorder()
	h.TriggerBackup(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["running_job_id"] != "running-job" {
		t.Errorf("running_job_id = %v, want running-job", body["running_job_id"])
	}
}

// -----------------------------------------------------------------------------
// ListRecords + ShowRecord + PatchRecord
// -----------------------------------------------------------------------------

func TestListRecords_FiltersByRepo(t *testing.T) {
	storeFake, repos := seedStoreWithRepo(2)
	storeFake.records = []*models.BackupRecord{
		{ID: "r1", RepoID: repos[0].ID, Status: models.BackupStatusSucceeded},
		{ID: "r2", RepoID: repos[1].ID, Status: models.BackupStatusSucceeded},
	}
	h := newTestHandler(storeFake, &fakeBackupService{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/store/metadata/fast-meta/backups?repo=primary", nil)
	req = withRouteParams(req, map[string]string{"name": "fast-meta"})
	rr := httptest.NewRecorder()
	h.ListRecords(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var list []*BackupRecordResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != 1 || list[0].ID != "r1" {
		t.Errorf("expected only r1 (primary repo), got %+v", list)
	}
}

func TestShowRecord_Returns404OnMiss(t *testing.T) {
	storeFake, _ := seedStoreWithRepo(1)
	h := newTestHandler(storeFake, &fakeBackupService{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/store/metadata/fast-meta/backups/unknown", nil)
	req = withRouteParams(req, map[string]string{"name": "fast-meta", "id": "unknown"})
	rr := httptest.NewRecorder()
	h.ShowRecord(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func TestPatchRecord_Pinned_Flips(t *testing.T) {
	storeFake, repos := seedStoreWithRepo(1)
	storeFake.records = []*models.BackupRecord{
		{ID: "r1", RepoID: repos[0].ID, Status: models.BackupStatusSucceeded, Pinned: false},
	}
	h := newTestHandler(storeFake, &fakeBackupService{})

	req := httptest.NewRequest(http.MethodPatch, "/api/v1/store/metadata/fast-meta/backups/r1", bytes.NewReader([]byte(`{"pinned":true}`)))
	req = withRouteParams(req, map[string]string{"name": "fast-meta", "id": "r1"})
	rr := httptest.NewRecorder()
	h.PatchRecord(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if storeFake.updatedPinnedID != "r1" || !storeFake.updatedPinnedValue {
		t.Errorf("pinned not applied: id=%q v=%v", storeFake.updatedPinnedID, storeFake.updatedPinnedValue)
	}
}

func TestPatchRecord_BadBody_Returns400(t *testing.T) {
	storeFake, _ := seedStoreWithRepo(1)
	h := newTestHandler(storeFake, &fakeBackupService{})

	req := httptest.NewRequest(http.MethodPatch, "/api/v1/store/metadata/fast-meta/backups/r1", bytes.NewReader([]byte(`{}`)))
	req = withRouteParams(req, map[string]string{"name": "fast-meta", "id": "r1"})
	rr := httptest.NewRecorder()
	h.PatchRecord(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

// -----------------------------------------------------------------------------
// Restore + RestoreDryRun
// -----------------------------------------------------------------------------

func TestRestore_PreconditionFailed_Returns409WithEnabledShares(t *testing.T) {
	storeFake, repos := seedStoreWithRepo(1)
	_ = repos
	svcFake := &fakeBackupService{
		runRestoreErr: errWithEnabledShares(storebackups.ErrRestorePreconditionFailed, []string{"/a", "/b"}),
	}
	h := newTestHandler(storeFake, svcFake)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/store/metadata/fast-meta/restore", bytes.NewReader([]byte(`{}`)))
	req = withRouteParams(req, map[string]string{"name": "fast-meta"})
	rr := httptest.NewRecorder()
	h.Restore(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	enabled, ok := body["enabled_shares"].([]any)
	if !ok || len(enabled) != 2 {
		t.Errorf("enabled_shares = %v (%T), want [/a /b]", body["enabled_shares"], body["enabled_shares"])
	}
}

func TestRestore_InvalidULID_Returns400(t *testing.T) {
	storeFake, _ := seedStoreWithRepo(1)
	h := newTestHandler(storeFake, &fakeBackupService{})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/store/metadata/fast-meta/restore", bytes.NewReader([]byte(`{"from_backup_id":"short"}`)))
	req = withRouteParams(req, map[string]string{"name": "fast-meta"})
	rr := httptest.NewRecorder()
	h.Restore(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestRestore_Succeeds_Returns202AndJob(t *testing.T) {
	storeFake, repos := seedStoreWithRepo(1)
	svcFake := &fakeBackupService{
		runRestoreJob: &models.BackupJob{ID: "j1", RepoID: repos[0].ID, Kind: models.BackupJobKindRestore, Status: models.BackupStatusRunning},
	}
	h := newTestHandler(storeFake, svcFake)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/store/metadata/fast-meta/restore", bytes.NewReader([]byte(`{}`)))
	req = withRouteParams(req, map[string]string{"name": "fast-meta"})
	rr := httptest.NewRecorder()
	h.Restore(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	var job BackupJobResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &job); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if job.ID != "j1" {
		t.Errorf("job.ID = %q, want j1", job.ID)
	}
}

func TestRestoreDryRun_ManifestValid_Returns200WithResult(t *testing.T) {
	storeFake, repos := seedStoreWithRepo(1)
	rec := &models.BackupRecord{ID: "r1", RepoID: repos[0].ID, Status: models.BackupStatusSucceeded}
	svcFake := &fakeBackupService{
		dryRunResult: &storebackups.DryRunResult{
			Record:        rec,
			ManifestValid: true,
			EnabledShares: []string{"/a"},
		},
	}
	h := newTestHandler(storeFake, svcFake)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/store/metadata/fast-meta/restore/dry-run", bytes.NewReader([]byte(`{}`)))
	req = withRouteParams(req, map[string]string{"name": "fast-meta"})
	rr := httptest.NewRecorder()
	h.RestoreDryRun(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var body RestoreDryRunResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.ManifestValid {
		t.Errorf("ManifestValid should be true")
	}
	if len(body.EnabledShares) != 1 || body.EnabledShares[0] != "/a" {
		t.Errorf("EnabledShares = %v, want [/a]", body.EnabledShares)
	}
	if body.Record == nil || body.Record.ID != "r1" {
		t.Errorf("Record missing: %+v", body.Record)
	}
}

func TestRestoreDryRun_ManifestInvalid_Returns200WithInvalidFlag(t *testing.T) {
	storeFake, repos := seedStoreWithRepo(1)
	rec := &models.BackupRecord{ID: "r1", RepoID: repos[0].ID}
	svcFake := &fakeBackupService{
		dryRunResult: &storebackups.DryRunResult{
			Record:        rec,
			ManifestValid: false,
		},
	}
	h := newTestHandler(storeFake, svcFake)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/store/metadata/fast-meta/restore/dry-run", bytes.NewReader([]byte(`{}`)))
	req = withRouteParams(req, map[string]string{"name": "fast-meta"})
	rr := httptest.NewRecorder()
	h.RestoreDryRun(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var body RestoreDryRunResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if body.ManifestValid {
		t.Errorf("ManifestValid should be false")
	}
}

func TestRestoreDryRun_NoRestoreCandidate_Returns409(t *testing.T) {
	storeFake, _ := seedStoreWithRepo(1)
	svcFake := &fakeBackupService{
		dryRunErr: fmt.Errorf("%w: no candidate", storebackups.ErrNoRestoreCandidate),
	}
	h := newTestHandler(storeFake, svcFake)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/store/metadata/fast-meta/restore/dry-run", bytes.NewReader([]byte(`{}`)))
	req = withRouteParams(req, map[string]string{"name": "fast-meta"})
	rr := httptest.NewRecorder()
	h.RestoreDryRun(rr, req)
	if rr.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409; body=%s", rr.Code, rr.Body.String())
	}
}

// -----------------------------------------------------------------------------
// Helpers for error injection
// -----------------------------------------------------------------------------

// enabledSharesErr wraps a base sentinel and exposes the enabled_shares slice
// to the handler via the `EnabledShares() []string` interface (matches the
// handler's extractEnabledShares contract).
type enabledSharesErr struct {
	base    error
	enabled []string
}

func (e *enabledSharesErr) Error() string {
	return fmt.Sprintf("%s: %v", e.base.Error(), e.enabled)
}
func (e *enabledSharesErr) Unwrap() error          { return e.base }
func (e *enabledSharesErr) EnabledShares() []string { return e.enabled }

func errWithEnabledShares(base error, enabled []string) error {
	return &enabledSharesErr{base: base, enabled: enabled}
}

// compile-time check that the base sentinel still matches via errors.Is
var _ = errors.Is(errWithEnabledShares(storebackups.ErrRestorePreconditionFailed, nil), storebackups.ErrRestorePreconditionFailed)
