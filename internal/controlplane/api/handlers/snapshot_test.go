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

	"github.com/marmos91/dittofs/pkg/controlplane/api/dto"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
)

// fakeSnapshotRuntime is a minimal SnapshotRuntime test double. Each
// field overrides the corresponding method; the zero value returns nil.
type fakeSnapshotRuntime struct {
	createFn  func(ctx context.Context, share string, opts runtime.CreateSnapshotOpts) (string, error)
	waitFn    func(ctx context.Context, share, snapID string) (*models.Snapshot, error)
	restoreFn func(ctx context.Context, share, snapID string, opts runtime.RestoreSnapshotOpts) (string, error)
	getFn     func(ctx context.Context, share, snapID string) (*models.Snapshot, error)
	listFn    func(ctx context.Context, share string) ([]*models.Snapshot, error)
	deleteFn  func(ctx context.Context, share, snapID string) error
}

func (f *fakeSnapshotRuntime) CreateSnapshot(ctx context.Context, share string, opts runtime.CreateSnapshotOpts) (string, error) {
	if f.createFn != nil {
		return f.createFn(ctx, share, opts)
	}
	return "", nil
}
func (f *fakeSnapshotRuntime) WaitForSnapshot(ctx context.Context, share, snapID string) (*models.Snapshot, error) {
	if f.waitFn != nil {
		return f.waitFn(ctx, share, snapID)
	}
	return nil, nil
}
func (f *fakeSnapshotRuntime) RestoreSnapshot(ctx context.Context, share, snapID string, opts runtime.RestoreSnapshotOpts) (string, error) {
	if f.restoreFn != nil {
		return f.restoreFn(ctx, share, snapID, opts)
	}
	return "", nil
}
func (f *fakeSnapshotRuntime) GetSnapshot(ctx context.Context, share, snapID string) (*models.Snapshot, error) {
	if f.getFn != nil {
		return f.getFn(ctx, share, snapID)
	}
	return nil, nil
}
func (f *fakeSnapshotRuntime) ListSnapshots(ctx context.Context, share string) ([]*models.Snapshot, error) {
	if f.listFn != nil {
		return f.listFn(ctx, share)
	}
	return nil, nil
}
func (f *fakeSnapshotRuntime) DeleteSnapshot(ctx context.Context, share, snapID string) error {
	if f.deleteFn != nil {
		return f.deleteFn(ctx, share, snapID)
	}
	return nil
}

// newSnapshotRouter wraps the SnapshotHandler in a chi router that
// matches the production route layout, so chi.URLParam resolves
// correctly under httptest.
func newSnapshotRouter(h *SnapshotHandler) http.Handler {
	r := chi.NewRouter()
	r.Route("/api/v1/shares", func(r chi.Router) {
		r.Route("/{name}/snapshots", func(r chi.Router) {
			r.Post("/", h.Create)
			r.Get("/", h.List)
			r.Get("/{id}", h.Get)
			r.Delete("/{id}", h.Delete)
			r.Post("/{id}/restore", h.Restore)
		})
	})
	return r
}

func TestSnapshotHandler_Create_HappyPath(t *testing.T) {
	fake := &fakeSnapshotRuntime{
		createFn: func(_ context.Context, share string, opts runtime.CreateSnapshotOpts) (string, error) {
			if share != "/data" {
				t.Fatalf("share = %q, want /data", share)
			}
			if !opts.NoVerify {
				t.Fatalf("opts.NoVerify = false, want true")
			}
			return "snap-1", nil
		},
	}
	h := NewSnapshotHandler(fake, 30*time.Second, nil)
	body := bytes.NewBufferString(`{"no_verify":true}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/shares/data/snapshots/", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	newSnapshotRouter(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: body=%s", rr.Code, rr.Body.String())
	}
	if loc := rr.Header().Get("Location"); loc != "/api/v1/shares/%2Fdata/snapshots/snap-1" {
		t.Fatalf("Location = %q, want /api/v1/shares/%%2Fdata/snapshots/snap-1", loc)
	}
	var got dto.CreateSnapshotResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.SnapshotID != "snap-1" || got.Share != "/data" {
		t.Fatalf("body = %+v, want {snap-1, /data}", got)
	}
}

// TestSnapshotHandler_Create_ForwardsName asserts the request body's name
// is forwarded to CreateSnapshotOpts.Name (was previously dropped).
func TestSnapshotHandler_Create_ForwardsName(t *testing.T) {
	var gotName string
	fake := &fakeSnapshotRuntime{
		createFn: func(_ context.Context, _ string, opts runtime.CreateSnapshotOpts) (string, error) {
			gotName = opts.Name
			return "snap-1", nil
		},
	}
	h := NewSnapshotHandler(fake, 30*time.Second, nil)
	body := bytes.NewBufferString(`{"name":"weekly"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/shares/data/snapshots/", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	newSnapshotRouter(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: body=%s", rr.Code, rr.Body.String())
	}
	if gotName != "weekly" {
		t.Fatalf("opts.Name = %q, want weekly", gotName)
	}
}

// TestSnapshotHandler_Get_IncludesName asserts the model's Name is surfaced
// in the wire DTO on the show endpoint.
func TestSnapshotHandler_Get_IncludesName(t *testing.T) {
	fake := &fakeSnapshotRuntime{
		getFn: func(_ context.Context, _, snapID string) (*models.Snapshot, error) {
			return &models.Snapshot{ID: snapID, Name: "weekly", ShareName: "/data", State: models.StateReady}, nil
		},
	}
	h := NewSnapshotHandler(fake, 30*time.Second, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/shares/data/snapshots/snap-1", nil)
	rr := httptest.NewRecorder()
	newSnapshotRouter(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var got dto.Snapshot
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != "weekly" {
		t.Fatalf("name = %q, want weekly", got.Name)
	}
}

func TestSnapshotHandler_List_EmptyArrayNotNull(t *testing.T) {
	fake := &fakeSnapshotRuntime{
		listFn: func(_ context.Context, _ string) ([]*models.Snapshot, error) {
			return []*models.Snapshot{}, nil
		},
	}
	h := NewSnapshotHandler(fake, 30*time.Second, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/shares/data/snapshots/", nil)
	rr := httptest.NewRecorder()
	newSnapshotRouter(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := strings.TrimSpace(rr.Body.String()); got != "[]" {
		t.Fatalf("body = %q, want []", got)
	}
}

func TestSnapshotHandler_List_PopulatedRecord(t *testing.T) {
	fake := &fakeSnapshotRuntime{
		listFn: func(_ context.Context, _ string) ([]*models.Snapshot, error) {
			return []*models.Snapshot{
				{ID: "snap-a", ShareName: "/data", State: models.StateReady, RemoteDurable: true},
				{ID: "snap-b", ShareName: "/data", State: models.StateCreating},
			}, nil
		},
	}
	h := NewSnapshotHandler(fake, 30*time.Second, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/shares/data/snapshots/", nil)
	rr := httptest.NewRecorder()
	newSnapshotRouter(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var got []dto.Snapshot
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].ID != "snap-a" || !got[0].RemoteDurable {
		t.Fatalf("got[0] = %+v", got[0])
	}
}

func TestSnapshotHandler_Get_HappyPath(t *testing.T) {
	fake := &fakeSnapshotRuntime{
		getFn: func(_ context.Context, _, snapID string) (*models.Snapshot, error) {
			return &models.Snapshot{ID: snapID, ShareName: "/data", State: models.StateReady, RemoteDurable: true}, nil
		},
	}
	h := NewSnapshotHandler(fake, 30*time.Second, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/shares/data/snapshots/snap-1", nil)
	rr := httptest.NewRecorder()
	newSnapshotRouter(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var got dto.Snapshot
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != "snap-1" || got.State != models.StateReady {
		t.Fatalf("body = %+v", got)
	}
}

func TestSnapshotHandler_Get_NotFound(t *testing.T) {
	fake := &fakeSnapshotRuntime{
		getFn: func(_ context.Context, _, _ string) (*models.Snapshot, error) {
			return nil, models.ErrSnapshotNotFound
		},
	}
	h := NewSnapshotHandler(fake, 30*time.Second, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/shares/data/snapshots/missing", nil)
	rr := httptest.NewRecorder()
	newSnapshotRouter(h).ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestSnapshotHandler_Delete_NoContent(t *testing.T) {
	fake := &fakeSnapshotRuntime{
		deleteFn: func(_ context.Context, _, _ string) error { return nil },
	}
	h := NewSnapshotHandler(fake, 30*time.Second, nil)
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/shares/data/snapshots/snap-1", nil)
	rr := httptest.NewRecorder()
	newSnapshotRouter(h).ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestSnapshotHandler_Delete_NotFound(t *testing.T) {
	fake := &fakeSnapshotRuntime{
		deleteFn: func(_ context.Context, _, _ string) error { return models.ErrSnapshotNotFound },
	}
	h := NewSnapshotHandler(fake, 30*time.Second, nil)
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/shares/data/snapshots/missing", nil)
	rr := httptest.NewRecorder()
	newSnapshotRouter(h).ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestSnapshotHandler_Restore_HappyPath(t *testing.T) {
	fake := &fakeSnapshotRuntime{
		restoreFn: func(_ context.Context, share, snapID string, opts runtime.RestoreSnapshotOpts) (string, error) {
			if share != "/data" || snapID != "snap-1" || !opts.AllowNonDurable {
				t.Fatalf("restore args = (%q, %q, %+v)", share, snapID, opts)
			}
			return "safety-9", nil
		},
	}
	h := NewSnapshotHandler(fake, 30*time.Second, nil)
	body := bytes.NewBufferString(`{"allow_non_durable":true}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/shares/data/snapshots/snap-1/restore", body)
	rr := httptest.NewRecorder()
	newSnapshotRouter(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: body=%s", rr.Code, rr.Body.String())
	}
	var got dto.RestoreSnapshotResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.SafetySnapshotID != "safety-9" {
		t.Fatalf("safety_snapshot_id = %q, want safety-9 (sourced directly from runtime return)", got.SafetySnapshotID)
	}
	if got.SnapshotID != "snap-1" || got.Share != "/data" {
		t.Fatalf("body = %+v", got)
	}
}

func TestSnapshotHandler_Restore_ContextTimeout(t *testing.T) {
	gotCtxErr := make(chan error, 1)
	fake := &fakeSnapshotRuntime{
		restoreFn: func(ctx context.Context, _, _ string, _ runtime.RestoreSnapshotOpts) (string, error) {
			select {
			case <-ctx.Done():
				gotCtxErr <- ctx.Err()
				return "", ctx.Err()
			case <-time.After(50 * time.Millisecond):
				gotCtxErr <- nil
				return "should-not-happen", nil
			}
		},
	}
	// 1ms timeout — restoreFn's 50ms branch should never complete.
	h := NewSnapshotHandler(fake, 1*time.Millisecond, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/shares/data/snapshots/snap-1/restore", nil)
	rr := httptest.NewRecorder()
	newSnapshotRouter(h).ServeHTTP(rr, req)

	select {
	case err := <-gotCtxErr:
		if err == nil {
			t.Fatalf("restoreFn finished without ctx cancellation; got nil")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("ctx err = %v, want DeadlineExceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("restoreFn never returned within 1s")
	}
}

// TestMapSnapshotError_SentinelTable asserts every typed sentinel maps to
// the canonical HTTP status code listed in the plan's mapping table.
func TestMapSnapshotError_SentinelTable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"SnapshotNotFound", models.ErrSnapshotNotFound, http.StatusNotFound},
		{"ShareNotFound", shares.ErrShareNotFound, http.StatusNotFound},
		{"ShareEnabled", models.ErrShareEnabled, http.StatusConflict},
		{"SnapshotNotDurable", models.ErrSnapshotNotDurable, http.StatusPreconditionFailed},
		{"RetryTargetNotFound", models.ErrSnapshotRetryTargetNotFound, http.StatusNotFound},
		{"RetryTargetNotFailed", models.ErrSnapshotRetryTargetNotFailed, http.StatusConflict},
		{"StateConflict", models.ErrSnapshotStateConflict, http.StatusConflict},
		{"DrainTimeout", models.ErrSnapshotDrainTimeout, http.StatusGatewayTimeout},
		{"MetadataDumpMissing", models.ErrSnapshotMetadataDumpMissing, http.StatusInternalServerError},
		{"MetadataStoreNotResetable", models.ErrMetadataStoreNotResetable, http.StatusInternalServerError},
		{"BackupFailed", models.ErrSnapshotBackupFailed, http.StatusInternalServerError},
		{"VerifyFailed", models.ErrSnapshotVerifyFailed, http.StatusInternalServerError},
		{"RestoreSafetySnapFailed", models.ErrRestoreSafetySnapFailed, http.StatusInternalServerError},
		{"RestoreAborted", models.ErrRestoreAborted, http.StatusInternalServerError},
		{"RestoreVerifyFailed", models.ErrRestoreVerifyFailed, http.StatusInternalServerError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			// Wrap once to exercise errors.Is matching (the runtime
			// always wraps with %w + context).
			wrapped := fmt.Errorf("layer: %w", tc.err)
			if !mapSnapshotError(rr, wrapped) {
				t.Fatalf("mapSnapshotError returned false; want true")
			}
			if rr.Code != tc.want {
				t.Fatalf("status = %d, want %d (body=%s)", rr.Code, tc.want, rr.Body.String())
			}
		})
	}
}

func TestMapSnapshotError_NilReturnsFalse(t *testing.T) {
	rr := httptest.NewRecorder()
	if mapSnapshotError(rr, nil) {
		t.Fatalf("mapSnapshotError(nil) = true, want false")
	}
	if rr.Code != 200 && rr.Code != 0 {
		t.Fatalf("status = %d, want untouched", rr.Code)
	}
}

func TestMapSnapshotError_UnknownReturnsFalse(t *testing.T) {
	rr := httptest.NewRecorder()
	if mapSnapshotError(rr, errors.New("some unknown error")) {
		t.Fatalf("mapSnapshotError(unknown) = true, want false")
	}
}

// TestSnapshotHandler_Restore_SanitizedMessage asserts the 500 body for
// a wrapped sentinel does not leak the underlying error message.
func TestSnapshotHandler_Restore_SanitizedMessage(t *testing.T) {
	leaky := errors.New("internal path /var/lib/dittofs/secret leaked here")
	fake := &fakeSnapshotRuntime{
		restoreFn: func(_ context.Context, _, _ string, _ runtime.RestoreSnapshotOpts) (string, error) {
			return "safety-1", fmt.Errorf("wrapped: %w: %v", models.ErrRestoreAborted, leaky)
		},
	}
	h := NewSnapshotHandler(fake, 30*time.Second, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/shares/data/snapshots/snap-1/restore", nil)
	rr := httptest.NewRecorder()
	newSnapshotRouter(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "secret") || strings.Contains(rr.Body.String(), "/var/lib") {
		t.Fatalf("body leaks internal detail: %s", rr.Body.String())
	}
}
