package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/storebackups"
)

func TestCreateRepo_ValidPayload_Returns201(t *testing.T) {
	storeFake, _ := seedStoreWithRepo(0)
	svcFake := &fakeBackupService{}
	h := newTestHandler(storeFake, svcFake)

	body := []byte(`{"name":"primary","kind":"local","config":{"path":"/tmp/bk"},"schedule":"0 0 * * *","keep_count":7}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/store/metadata/fast-meta/repos", bytes.NewReader(body))
	req = withRouteParams(req, map[string]string{"name": "fast-meta"})
	rr := httptest.NewRecorder()
	h.CreateRepo(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	if svcFake.validatedExpr != "0 0 * * *" {
		t.Errorf("ValidateSchedule not called or wrong expr: %q", svcFake.validatedExpr)
	}
	var resp BackupRepoResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Name != "primary" || resp.Kind != "local" {
		t.Errorf("response = %+v", resp)
	}
}

func TestCreateRepo_InvalidSchedule_Returns400(t *testing.T) {
	storeFake, _ := seedStoreWithRepo(0)
	svcFake := &fakeBackupService{validateScheduleErr: storebackups.ErrScheduleInvalid}
	h := newTestHandler(storeFake, svcFake)

	body := []byte(`{"name":"primary","kind":"local","schedule":"!@#"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/store/metadata/fast-meta/repos", bytes.NewReader(body))
	req = withRouteParams(req, map[string]string{"name": "fast-meta"})
	rr := httptest.NewRecorder()
	h.CreateRepo(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestListRepos_ForStore(t *testing.T) {
	storeFake, _ := seedStoreWithRepo(2)
	h := newTestHandler(storeFake, &fakeBackupService{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/store/metadata/fast-meta/repos", nil)
	req = withRouteParams(req, map[string]string{"name": "fast-meta"})
	rr := httptest.NewRecorder()
	h.ListRepos(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var list []*BackupRepoResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("expected 2 repos, got %d", len(list))
	}
}

func TestGetRepo_Returns404OnMiss(t *testing.T) {
	storeFake, _ := seedStoreWithRepo(0)
	h := newTestHandler(storeFake, &fakeBackupService{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/store/metadata/fast-meta/repos/unknown", nil)
	req = withRouteParams(req, map[string]string{"name": "fast-meta", "repo": "unknown"})
	rr := httptest.NewRecorder()
	h.GetRepo(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func TestPatchRepo_PartialUpdate(t *testing.T) {
	storeFake, _ := seedStoreWithRepo(1)
	// Seed existing keep_count
	original := 3
	storeFake.repos[0].KeepCount = &original
	h := newTestHandler(storeFake, &fakeBackupService{})

	body := []byte(`{"keep_count": 10}`)
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/store/metadata/fast-meta/repos/primary", bytes.NewReader(body))
	req = withRouteParams(req, map[string]string{"name": "fast-meta", "repo": "primary"})
	rr := httptest.NewRecorder()
	h.PatchRepo(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if storeFake.repos[0].KeepCount == nil || *storeFake.repos[0].KeepCount != 10 {
		t.Errorf("KeepCount not updated: %v", storeFake.repos[0].KeepCount)
	}
	// Name should not have changed
	if storeFake.repos[0].Name != "primary" {
		t.Errorf("Name mutated to %q — partial update violated", storeFake.repos[0].Name)
	}
}

func TestDeleteRepo_Default_RemovesRow(t *testing.T) {
	storeFake, _ := seedStoreWithRepo(1)
	h := newTestHandler(storeFake, &fakeBackupService{})

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/store/metadata/fast-meta/repos/primary", nil)
	req = withRouteParams(req, map[string]string{"name": "fast-meta", "repo": "primary"})
	rr := httptest.NewRecorder()
	h.DeleteRepo(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	if storeFake.deleteRepoCalledID != "repo-0" {
		t.Errorf("DeleteBackupRepo not called for repo-0; got %q", storeFake.deleteRepoCalledID)
	}
}

// -----------------------------------------------------------------------------
// DeleteRepo with purge_archives=true
// -----------------------------------------------------------------------------

type fakeDest struct {
	deletes map[string]error // id -> error to return
	closed  bool
}

func (d *fakeDest) Delete(_ context.Context, id string) error {
	if err, ok := d.deletes[id]; ok {
		return err
	}
	return nil
}
func (d *fakeDest) Close() error { d.closed = true; return nil }

func TestDeleteRepo_PurgeArchives_CascadesDestination(t *testing.T) {
	storeFake, repos := seedStoreWithRepo(1)
	storeFake.records = []*models.BackupRecord{
		{ID: "r1", RepoID: repos[0].ID, Status: models.BackupStatusSucceeded},
		{ID: "r2", RepoID: repos[0].ID, Status: models.BackupStatusFailed},
		{ID: "r3", RepoID: repos[0].ID, Status: models.BackupStatusSucceeded},
	}
	// r2 fails to delete; rest succeed.
	dest := &fakeDest{deletes: map[string]error{"r2": errors.New("storage error")}}
	factory := func(_ context.Context, _ *models.BackupRepo) (BackupDestinationDeleter, error) {
		return dest, nil
	}
	h := NewBackupHandler(storeFake, &fakeBackupService{}, factory)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/store/metadata/fast-meta/repos/primary?purge_archives=true", nil)
	req = withRouteParams(req, map[string]string{"name": "fast-meta", "repo": "primary"})
	rr := httptest.NewRecorder()
	h.DeleteRepo(rr, req)

	// Partial failure → 200 + problem body, repo row preserved.
	if rr.Code != http.StatusOK {
		t.Fatalf("partial-failure status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var body BackupRepoPurgeProblem
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.FailedRecordIDs) != 1 || body.FailedRecordIDs[0] != "r2" {
		t.Errorf("FailedRecordIDs = %v, want [r2]", body.FailedRecordIDs)
	}
	// Repo row must NOT be deleted on partial failure.
	if storeFake.deleteRepoCalledID != "" {
		t.Errorf("DeleteBackupRepo should NOT be called on partial failure; was called with %q", storeFake.deleteRepoCalledID)
	}
	if !dest.closed {
		t.Errorf("Destination.Close should have been called")
	}
}
