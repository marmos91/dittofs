package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/marmos91/dittofs/pkg/controlplane/api/dto"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

type fakeSnapshotPolicyRuntime struct {
	getFn    func(ctx context.Context, share string) (*models.SnapshotPolicy, error)
	listFn   func(ctx context.Context) ([]*models.SnapshotPolicy, error)
	upsertFn func(ctx context.Context, p *models.SnapshotPolicy) error
	deleteFn func(ctx context.Context, share string) error
	runFn    func(ctx context.Context, share string) (string, error)

	upserted *models.SnapshotPolicy
}

func (f *fakeSnapshotPolicyRuntime) GetSnapshotPolicy(ctx context.Context, share string) (*models.SnapshotPolicy, error) {
	if f.getFn != nil {
		return f.getFn(ctx, share)
	}
	return nil, nil
}
func (f *fakeSnapshotPolicyRuntime) ListSnapshotPolicies(ctx context.Context) ([]*models.SnapshotPolicy, error) {
	if f.listFn != nil {
		return f.listFn(ctx)
	}
	return nil, nil
}
func (f *fakeSnapshotPolicyRuntime) UpsertSnapshotPolicy(ctx context.Context, p *models.SnapshotPolicy) error {
	f.upserted = p
	if f.upsertFn != nil {
		return f.upsertFn(ctx, p)
	}
	return nil
}
func (f *fakeSnapshotPolicyRuntime) DeleteSnapshotPolicy(ctx context.Context, share string) error {
	if f.deleteFn != nil {
		return f.deleteFn(ctx, share)
	}
	return nil
}
func (f *fakeSnapshotPolicyRuntime) RunSnapshotPolicyNow(ctx context.Context, share string) (string, error) {
	if f.runFn != nil {
		return f.runFn(ctx, share)
	}
	return "", nil
}

func newPolicyRouter(h *SnapshotPolicyHandler) http.Handler {
	r := chi.NewRouter()
	r.Route("/api/v1", func(r chi.Router) {
		r.Route("/shares/{name}/snapshot-policy", func(r chi.Router) {
			r.Put("/", h.Upsert)
			r.Get("/", h.Get)
			r.Delete("/", h.Delete)
			r.Post("/run", h.Run)
		})
		r.Get("/snapshot-policies", h.List)
	})
	return r
}

func TestSnapshotPolicyHandler_Upsert_OK(t *testing.T) {
	fake := &fakeSnapshotPolicyRuntime{
		getFn: func(ctx context.Context, share string) (*models.SnapshotPolicy, error) {
			return &models.SnapshotPolicy{ShareName: share, Enabled: true, Interval: 6 * time.Hour, KeepLast: 3, TTL: 48 * time.Hour}, nil
		},
	}
	h := NewSnapshotPolicyHandler(fake)
	body, _ := json.Marshal(dto.UpsertSnapshotPolicyRequest{Interval: "@daily", KeepLast: 3, TTL: "48h"})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/shares/data/snapshot-policy", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	newPolicyRouter(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	if fake.upserted == nil || fake.upserted.Interval != 24*time.Hour {
		t.Fatalf("@daily not parsed to 24h: %+v", fake.upserted)
	}
}

func TestSnapshotPolicyHandler_Upsert_InvalidInterval(t *testing.T) {
	h := NewSnapshotPolicyHandler(&fakeSnapshotPolicyRuntime{})
	body, _ := json.Marshal(dto.UpsertSnapshotPolicyRequest{Interval: "notaduration"})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/shares/data/snapshot-policy", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	newPolicyRouter(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestSnapshotPolicyHandler_Upsert_NegativeKeepLast(t *testing.T) {
	h := NewSnapshotPolicyHandler(&fakeSnapshotPolicyRuntime{})
	body, _ := json.Marshal(dto.UpsertSnapshotPolicyRequest{Interval: "24h", KeepLast: -1})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/shares/data/snapshot-policy", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	newPolicyRouter(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestSnapshotPolicyHandler_Upsert_ShareNotFound(t *testing.T) {
	fake := &fakeSnapshotPolicyRuntime{
		upsertFn: func(ctx context.Context, p *models.SnapshotPolicy) error { return models.ErrShareNotFound },
	}
	h := NewSnapshotPolicyHandler(fake)
	body, _ := json.Marshal(dto.UpsertSnapshotPolicyRequest{Interval: "24h"})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/shares/data/snapshot-policy", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	newPolicyRouter(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestSnapshotPolicyHandler_Get_NotFound(t *testing.T) {
	fake := &fakeSnapshotPolicyRuntime{
		getFn: func(ctx context.Context, share string) (*models.SnapshotPolicy, error) {
			return nil, models.ErrSnapshotPolicyNotFound
		},
	}
	h := NewSnapshotPolicyHandler(fake)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/shares/data/snapshot-policy", nil)
	rr := httptest.NewRecorder()
	newPolicyRouter(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestSnapshotPolicyHandler_Run_Conflict(t *testing.T) {
	fake := &fakeSnapshotPolicyRuntime{
		runFn: func(ctx context.Context, share string) (string, error) { return "", models.ErrSnapshotStateConflict },
	}
	h := NewSnapshotPolicyHandler(fake)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/shares/data/snapshot-policy/run", nil)
	rr := httptest.NewRecorder()
	newPolicyRouter(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rr.Code)
	}
}

func TestSnapshotPolicyHandler_Run_OK(t *testing.T) {
	fake := &fakeSnapshotPolicyRuntime{
		runFn: func(ctx context.Context, share string) (string, error) { return "snap-1", nil },
	}
	h := NewSnapshotPolicyHandler(fake)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/shares/data/snapshot-policy/run", nil)
	rr := httptest.NewRecorder()
	newPolicyRouter(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body=%s)", rr.Code, rr.Body.String())
	}
	var resp dto.CreateSnapshotResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.SnapshotID != "snap-1" {
		t.Fatalf("snapshot_id = %q, want snap-1", resp.SnapshotID)
	}
}

func TestSnapshotPolicyHandler_List_Empty(t *testing.T) {
	h := NewSnapshotPolicyHandler(&fakeSnapshotPolicyRuntime{
		listFn: func(ctx context.Context) ([]*models.SnapshotPolicy, error) { return nil, nil },
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/snapshot-policies", nil)
	rr := httptest.NewRecorder()
	newPolicyRouter(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := bytes.TrimSpace(rr.Body.Bytes()); string(got) != "[]" {
		t.Fatalf("body = %s, want []", got)
	}
}
