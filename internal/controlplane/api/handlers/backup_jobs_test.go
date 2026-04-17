package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/storebackups"
)

func TestListJobs_FilterStatusKindLimit(t *testing.T) {
	storeFake, repos := seedStoreWithRepo(1)
	storeFake.jobs = []*models.BackupJob{
		{ID: "j1", RepoID: repos[0].ID, Kind: models.BackupJobKindBackup, Status: models.BackupStatusRunning},
		{ID: "j2", RepoID: repos[0].ID, Kind: models.BackupJobKindBackup, Status: models.BackupStatusSucceeded},
	}
	h := newTestHandler(storeFake, &fakeBackupService{})

	t.Run("valid filters", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/store/metadata/fast-meta/backup-jobs?status=running&kind=backup&limit=10", nil)
		req = withRouteParams(req, map[string]string{"name": "fast-meta"})
		rr := httptest.NewRecorder()
		h.ListJobs(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
		}
		if storeFake.lastListFilter.Status != models.BackupStatusRunning {
			t.Errorf("status filter = %q, want running", storeFake.lastListFilter.Status)
		}
		if storeFake.lastListFilter.Kind != models.BackupJobKindBackup {
			t.Errorf("kind filter = %q, want backup", storeFake.lastListFilter.Kind)
		}
		if storeFake.lastListFilter.Limit != 10 {
			t.Errorf("limit = %d, want 10", storeFake.lastListFilter.Limit)
		}
	})

	t.Run("invalid status", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/store/metadata/fast-meta/backup-jobs?status=bogus", nil)
		req = withRouteParams(req, map[string]string{"name": "fast-meta"})
		rr := httptest.NewRecorder()
		h.ListJobs(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("invalid status should 400, got %d", rr.Code)
		}
	})

	t.Run("invalid kind", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/store/metadata/fast-meta/backup-jobs?kind=bogus", nil)
		req = withRouteParams(req, map[string]string{"name": "fast-meta"})
		rr := httptest.NewRecorder()
		h.ListJobs(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("invalid kind should 400, got %d", rr.Code)
		}
	})

	t.Run("invalid limit", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/store/metadata/fast-meta/backup-jobs?limit=not-a-number", nil)
		req = withRouteParams(req, map[string]string{"name": "fast-meta"})
		rr := httptest.NewRecorder()
		h.ListJobs(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("invalid limit should 400, got %d", rr.Code)
		}
	})
}

func TestListJobs_FilterByRepo(t *testing.T) {
	storeFake, repos := seedStoreWithRepo(1)
	h := newTestHandler(storeFake, &fakeBackupService{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/store/metadata/fast-meta/backup-jobs?repo=primary", nil)
	req = withRouteParams(req, map[string]string{"name": "fast-meta"})
	rr := httptest.NewRecorder()
	h.ListJobs(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if storeFake.lastListFilter.RepoID != repos[0].ID {
		t.Errorf("RepoID filter = %q, want %q", storeFake.lastListFilter.RepoID, repos[0].ID)
	}
}

func TestShowJob_Returns404OnMiss(t *testing.T) {
	storeFake, _ := seedStoreWithRepo(1)
	h := newTestHandler(storeFake, &fakeBackupService{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/store/metadata/fast-meta/backup-jobs/unknown", nil)
	req = withRouteParams(req, map[string]string{"name": "fast-meta", "id": "unknown"})
	rr := httptest.NewRecorder()
	h.GetJob(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func TestCancelJob_Running_Returns202(t *testing.T) {
	storeFake, repos := seedStoreWithRepo(1)
	storeFake.jobs = []*models.BackupJob{
		{ID: "j1", RepoID: repos[0].ID, Status: models.BackupStatusRunning},
	}
	svcFake := &fakeBackupService{}
	h := newTestHandler(storeFake, svcFake)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/store/metadata/fast-meta/backup-jobs/j1/cancel", bytes.NewReader(nil))
	req = withRouteParams(req, map[string]string{"name": "fast-meta", "id": "j1"})
	rr := httptest.NewRecorder()
	h.CancelJob(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	if svcFake.cancelJobID != "j1" {
		t.Errorf("CancelBackupJob not called for j1; got %q", svcFake.cancelJobID)
	}
}

func TestCancelJob_Terminal_Idempotent(t *testing.T) {
	storeFake, repos := seedStoreWithRepo(1)
	storeFake.jobs = []*models.BackupJob{
		{ID: "j1", RepoID: repos[0].ID, Status: models.BackupStatusSucceeded},
	}
	svcFake := &fakeBackupService{}
	h := newTestHandler(storeFake, svcFake)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/store/metadata/fast-meta/backup-jobs/j1/cancel", nil)
	req = withRouteParams(req, map[string]string{"name": "fast-meta", "id": "j1"})
	rr := httptest.NewRecorder()
	h.CancelJob(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("terminal cancel status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if svcFake.cancelJobID != "" {
		t.Errorf("CancelBackupJob should NOT have been dispatched for terminal job")
	}
	var job BackupJobResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &job); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if job.ID != "j1" || job.Status != "succeeded" {
		t.Errorf("unexpected job response: %+v", job)
	}
}

func TestCancelJob_RegistryMiss_Returns200Idempotent(t *testing.T) {
	// DB row exists and is running, but svc.Cancel returns ErrBackupJobNotFound
	// (race: the executor wound down between the read and the dispatch).
	storeFake, repos := seedStoreWithRepo(1)
	storeFake.jobs = []*models.BackupJob{
		{ID: "j1", RepoID: repos[0].ID, Status: models.BackupStatusRunning},
	}
	svcFake := &fakeBackupService{cancelErr: storebackups.ErrBackupJobNotFound}
	h := newTestHandler(storeFake, svcFake)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/store/metadata/fast-meta/backup-jobs/j1/cancel", nil)
	req = withRouteParams(req, map[string]string{"name": "fast-meta", "id": "j1"})
	rr := httptest.NewRecorder()
	h.CancelJob(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("registry-miss cancel should 200 per D-45, got %d; body=%s", rr.Code, rr.Body.String())
	}
}

func TestCancelJob_TrulyUnknown_Returns404(t *testing.T) {
	storeFake, _ := seedStoreWithRepo(1)
	h := newTestHandler(storeFake, &fakeBackupService{})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/store/metadata/fast-meta/backup-jobs/unknown/cancel", nil)
	req = withRouteParams(req, map[string]string{"name": "fast-meta", "id": "unknown"})
	rr := httptest.NewRecorder()
	h.CancelJob(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}
