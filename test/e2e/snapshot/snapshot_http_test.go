//go:build e2e

package snapshot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/marmos91/dittofs/internal/controlplane/api/handlers"
	"github.com/marmos91/dittofs/pkg/controlplane/api/dto"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// fakeRuntime is the canonical SnapshotRuntime test double for the e2e
// HTTP suite. Each method is backed by a swappable hook so individual
// failure-mode tests inject the exact sentinel the handler is expected
// to translate.
type fakeRuntime struct {
	mu       sync.Mutex
	store    map[string]map[string]*models.Snapshot // share -> snapID -> snap
	idSeq    int
	hooks    fakeHooks
	restored []string // history of (snapID restored)
}

type fakeHooks struct {
	createErr        error
	waitErr          error
	restore          func(ctx context.Context, share, snapID string, opts runtime.RestoreSnapshotOpts) (string, error)
	getErr           error
	listErr          error
	deleteErr        error
	createSafetyName string // optional: name to apply to a "safety" snapshot create
}

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{store: make(map[string]map[string]*models.Snapshot)}
}

func (f *fakeRuntime) seedSnapshot(share string, snap *models.Snapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.store[share]; !ok {
		f.store[share] = map[string]*models.Snapshot{}
	}
	f.store[share][snap.ID] = snap
}

func (f *fakeRuntime) CreateSnapshot(_ context.Context, share string, _ runtime.CreateSnapshotOpts) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hooks.createErr != nil {
		return "", f.hooks.createErr
	}
	f.idSeq++
	id := fmt.Sprintf("snap-%d", f.idSeq)
	if _, ok := f.store[share]; !ok {
		f.store[share] = map[string]*models.Snapshot{}
	}
	f.store[share][id] = &models.Snapshot{
		ID:            id,
		ShareName:     share,
		State:         models.StateCreating,
		RemoteDurable: false,
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
	return id, nil
}

func (f *fakeRuntime) WaitForSnapshot(_ context.Context, share, snapID string) (*models.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hooks.waitErr != nil {
		return nil, f.hooks.waitErr
	}
	if s, ok := f.store[share][snapID]; ok {
		s.State = models.StateReady
		s.RemoteDurable = true
		return s, nil
	}
	return nil, models.ErrSnapshotNotFound
}

func (f *fakeRuntime) RestoreSnapshot(ctx context.Context, share, snapID string, opts runtime.RestoreSnapshotOpts) (string, error) {
	f.mu.Lock()
	hook := f.hooks.restore
	f.mu.Unlock()
	if hook != nil {
		return hook(ctx, share, snapID, opts)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	snap, ok := f.store[share][snapID]
	if !ok {
		return "", models.ErrSnapshotNotFound
	}
	if snap.State != models.StateReady {
		return "", fmt.Errorf("snap state=%q: %w", snap.State, models.ErrSnapshotStateConflict)
	}
	if !snap.RemoteDurable && !opts.AllowNonDurable {
		return "", fmt.Errorf("snap %q: %w", snapID, models.ErrSnapshotNotDurable)
	}
	f.idSeq++
	safetyID := fmt.Sprintf("safety-%d", f.idSeq)
	f.store[share][safetyID] = &models.Snapshot{
		ID:            safetyID,
		ShareName:     share,
		State:         models.StateReady,
		RemoteDurable: true,
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
	f.restored = append(f.restored, snapID)
	return safetyID, nil
}

func (f *fakeRuntime) GetSnapshot(_ context.Context, share, snapID string) (*models.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hooks.getErr != nil {
		return nil, f.hooks.getErr
	}
	if s, ok := f.store[share][snapID]; ok {
		return s, nil
	}
	return nil, models.ErrSnapshotNotFound
}

func (f *fakeRuntime) ListSnapshots(_ context.Context, share string) ([]*models.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hooks.listErr != nil {
		return nil, f.hooks.listErr
	}
	bucket, ok := f.store[share]
	if !ok {
		return []*models.Snapshot{}, nil
	}
	out := make([]*models.Snapshot, 0, len(bucket))
	for _, s := range bucket {
		out = append(out, s)
	}
	return out, nil
}

func (f *fakeRuntime) DeleteSnapshot(_ context.Context, share, snapID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hooks.deleteErr != nil {
		return f.hooks.deleteErr
	}
	if _, ok := f.store[share][snapID]; !ok {
		return models.ErrSnapshotNotFound
	}
	delete(f.store[share], snapID)
	return nil
}

// mountHandler returns an httptest server backed by a chi router that
// mirrors the production /api/v1/shares/{name}/snapshots layout. The
// JWT + RequireAdmin middleware is intentionally omitted — this suite
// covers handler behavior and routing, not auth (which has its own
// dedicated tests under internal/controlplane/api/middleware).
func mountHandler(t *testing.T, rt handlers.SnapshotRuntime, restoreTimeout time.Duration) (*httptest.Server, func()) {
	t.Helper()
	h := handlers.NewSnapshotHandler(rt, restoreTimeout, nil)
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
	srv := httptest.NewServer(r)
	return srv, srv.Close
}

func doRequest(t *testing.T, method, url string, body any) (*http.Response, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, raw
}

// TestSnapshotHTTP_HappyPath exercises the full lifecycle: create → list
// → get → restore → delete.
func TestSnapshotHTTP_HappyPath(t *testing.T) {
	fake := newFakeRuntime()
	// Pre-seed a ready, remote-durable snapshot so restore succeeds.
	fake.seedSnapshot("/data", &models.Snapshot{
		ID: "snap-existing", ShareName: "/data",
		State: models.StateReady, RemoteDurable: true,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	srv, cleanup := mountHandler(t, fake, 30*time.Second)
	defer cleanup()

	// Create
	resp, raw := doRequest(t, http.MethodPost, srv.URL+"/api/v1/shares/data/snapshots/", dto.CreateSnapshotRequest{})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create: status %d, want 202: %s", resp.StatusCode, raw)
	}
	if loc := resp.Header.Get("Location"); loc == "" {
		t.Fatalf("create: missing Location header")
	}
	var createBody dto.CreateSnapshotResponse
	if err := json.Unmarshal(raw, &createBody); err != nil {
		t.Fatalf("create decode: %v", err)
	}
	if createBody.SnapshotID == "" || createBody.Share != "/data" {
		t.Fatalf("create body = %+v", createBody)
	}

	// List
	resp, raw = doRequest(t, http.MethodGet, srv.URL+"/api/v1/shares/data/snapshots/", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: status %d, want 200: %s", resp.StatusCode, raw)
	}
	var listBody []dto.Snapshot
	if err := json.Unmarshal(raw, &listBody); err != nil {
		t.Fatalf("list decode: %v", err)
	}
	if len(listBody) < 2 {
		t.Fatalf("list len = %d, want >= 2 (seeded + created)", len(listBody))
	}

	// Get
	resp, raw = doRequest(t, http.MethodGet, srv.URL+"/api/v1/shares/data/snapshots/snap-existing", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get: status %d, want 200: %s", resp.StatusCode, raw)
	}
	var getBody dto.Snapshot
	if err := json.Unmarshal(raw, &getBody); err != nil {
		t.Fatalf("get decode: %v", err)
	}
	if getBody.ID != "snap-existing" {
		t.Fatalf("get id = %q", getBody.ID)
	}

	// Restore — assert safety_snapshot_id is non-empty (sourced from runtime return)
	resp, raw = doRequest(t, http.MethodPost,
		srv.URL+"/api/v1/shares/data/snapshots/snap-existing/restore",
		dto.RestoreSnapshotRequest{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("restore: status %d, want 200: %s", resp.StatusCode, raw)
	}
	var restoreBody dto.RestoreSnapshotResponse
	if err := json.Unmarshal(raw, &restoreBody); err != nil {
		t.Fatalf("restore decode: %v", err)
	}
	if restoreBody.SafetySnapshotID == "" {
		t.Fatalf("safety_snapshot_id is empty; restore handler must surface the runtime return value")
	}
	if restoreBody.SnapshotID != "snap-existing" {
		t.Fatalf("snapshot_id = %q, want snap-existing", restoreBody.SnapshotID)
	}

	// Delete
	resp, _ = doRequest(t, http.MethodDelete, srv.URL+"/api/v1/shares/data/snapshots/snap-existing", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: status %d, want 204", resp.StatusCode)
	}

	// Get after delete → 404
	resp, _ = doRequest(t, http.MethodGet, srv.URL+"/api/v1/shares/data/snapshots/snap-existing", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get-after-delete: status %d, want 404", resp.StatusCode)
	}
}

// TestSnapshotHTTP_RestoreFailureModes exercises all nine documented
// restore failure modes via the HTTP surface, using fault injection on
// the fake runtime so every mode is reachable from the memory backend.
func TestSnapshotHTTP_RestoreFailureModes(t *testing.T) {
	tests := []struct {
		name       string
		body       dto.RestoreSnapshotRequest
		setup      func(*fakeRuntime)
		wantStatus int
		wantInBody string
	}{
		{
			name: "1_share_enabled",
			setup: func(f *fakeRuntime) {
				f.seedSnapshot("/data", &models.Snapshot{
					ID: "snap-1", ShareName: "/data",
					State: models.StateReady, RemoteDurable: true,
				})
				f.hooks.restore = func(_ context.Context, _, _ string, _ runtime.RestoreSnapshotOpts) (string, error) {
					return "", fmt.Errorf("share enabled: %w", models.ErrShareEnabled)
				}
			},
			wantStatus: http.StatusConflict,
			wantInBody: "disable",
		},
		{
			name: "2_snapshot_not_found",
			setup: func(f *fakeRuntime) {
				f.hooks.restore = func(_ context.Context, _, _ string, _ runtime.RestoreSnapshotOpts) (string, error) {
					return "", models.ErrSnapshotNotFound
				}
			},
			wantStatus: http.StatusNotFound,
			wantInBody: "snapshot not found",
		},
		{
			name: "3_not_durable_no_force",
			setup: func(f *fakeRuntime) {
				f.seedSnapshot("/data", &models.Snapshot{
					ID: "snap-1", ShareName: "/data",
					State: models.StateReady, RemoteDurable: false,
				})
			},
			wantStatus: http.StatusPreconditionFailed,
			wantInBody: "allow_non_durable",
		},
		{
			name: "4_not_durable_with_force_succeeds",
			body: dto.RestoreSnapshotRequest{AllowNonDurable: true},
			setup: func(f *fakeRuntime) {
				f.seedSnapshot("/data", &models.Snapshot{
					ID: "snap-1", ShareName: "/data",
					State: models.StateReady, RemoteDurable: false,
				})
			},
			wantStatus: http.StatusOK,
			wantInBody: `"safety_snapshot_id":`,
		},
		{
			name: "5_metadata_dump_missing",
			setup: func(f *fakeRuntime) {
				f.hooks.restore = func(_ context.Context, _, _ string, _ runtime.RestoreSnapshotOpts) (string, error) {
					return "", fmt.Errorf("open dump: %w", models.ErrSnapshotMetadataDumpMissing)
				}
			},
			wantStatus: http.StatusInternalServerError,
			wantInBody: "artifacts missing",
		},
		{
			name: "6_metadata_store_not_resetable",
			setup: func(f *fakeRuntime) {
				f.hooks.restore = func(_ context.Context, _, _ string, _ runtime.RestoreSnapshotOpts) (string, error) {
					return "", fmt.Errorf("reset: %w", models.ErrMetadataStoreNotResetable)
				}
			},
			wantStatus: http.StatusInternalServerError,
			wantInBody: "does not support reset",
		},
		{
			name: "7_safety_snap_create_failed",
			setup: func(f *fakeRuntime) {
				f.hooks.restore = func(_ context.Context, _, _ string, _ runtime.RestoreSnapshotOpts) (string, error) {
					return "", fmt.Errorf("safety snap: %w", models.ErrRestoreSafetySnapFailed)
				}
			},
			wantStatus: http.StatusInternalServerError,
			wantInBody: "snapshot operation failed",
		},
		{
			name: "8_restore_aborted",
			setup: func(f *fakeRuntime) {
				f.hooks.restore = func(ctx context.Context, _, _ string, _ runtime.RestoreSnapshotOpts) (string, error) {
					// Simulate a mid-flight cancel by observing ctx; the
					// handler-side ctx is cancelled when the client gives up
					// or the per-request timeout fires.
					select {
					case <-ctx.Done():
					case <-time.After(10 * time.Millisecond):
					}
					return "safety-9", fmt.Errorf("reset aborted: %w", models.ErrRestoreAborted)
				}
			},
			wantStatus: http.StatusInternalServerError,
			wantInBody: "snapshot operation failed",
		},
		{
			name: "9_post_verify_failed",
			setup: func(f *fakeRuntime) {
				f.hooks.restore = func(_ context.Context, _, _ string, _ runtime.RestoreSnapshotOpts) (string, error) {
					return "safety-9", fmt.Errorf("post-verify: %w", models.ErrRestoreVerifyFailed)
				}
			},
			wantStatus: http.StatusInternalServerError,
			wantInBody: "snapshot operation failed",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeRuntime()
			tc.setup(fake)
			srv, cleanup := mountHandler(t, fake, 30*time.Second)
			defer cleanup()

			resp, raw := doRequest(t, http.MethodPost,
				srv.URL+"/api/v1/shares/data/snapshots/snap-1/restore", tc.body)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body=%s)", resp.StatusCode, tc.wantStatus, raw)
			}
			if tc.wantInBody != "" && !strings.Contains(string(raw), tc.wantInBody) {
				t.Fatalf("body does not contain %q\nbody=%s", tc.wantInBody, raw)
			}
		})
	}
}

// TestSnapshotHTTP_CreateFailureModes covers the create-path sentinels
// that did not fit the restore taxonomy.
func TestSnapshotHTTP_CreateFailureModes(t *testing.T) {
	tests := []struct {
		name       string
		body       dto.CreateSnapshotRequest
		setup      func(*fakeRuntime)
		wantStatus int
	}{
		{
			name: "retry_target_not_found",
			body: dto.CreateSnapshotRequest{RetryOf: "nope"},
			setup: func(f *fakeRuntime) {
				f.hooks.createErr = fmt.Errorf("retry of %q: %w", "nope", models.ErrSnapshotRetryTargetNotFound)
			},
			wantStatus: http.StatusNotFound,
		},
		{
			name: "retry_target_not_failed",
			body: dto.CreateSnapshotRequest{RetryOf: "wrong-state"},
			setup: func(f *fakeRuntime) {
				f.hooks.createErr = fmt.Errorf("retry of %q: %w", "wrong-state", models.ErrSnapshotRetryTargetNotFailed)
			},
			wantStatus: http.StatusConflict,
		},
		{
			name: "verify_failed_via_get",
			setup: func(f *fakeRuntime) {
				// CreateSnapshot succeeds (state=creating); a subsequent
				// GET surfaces state=failed once the orchestration goroutine
				// reports verify failure. The HTTP-layer contract here is:
				// GET reports state=failed when the snapshot has failed.
				f.seedSnapshot("/data", &models.Snapshot{
					ID: "snap-vf", ShareName: "/data",
					State: models.StateFailed, RemoteDurable: false,
				})
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeRuntime()
			tc.setup(fake)
			srv, cleanup := mountHandler(t, fake, 30*time.Second)
			defer cleanup()

			if tc.name == "verify_failed_via_get" {
				// Verify the failed-state propagation through GET.
				resp, raw := doRequest(t, http.MethodGet, srv.URL+"/api/v1/shares/data/snapshots/snap-vf", nil)
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("status = %d, want 200 (failed snap still listable): body=%s", resp.StatusCode, raw)
				}
				var got dto.Snapshot
				if err := json.Unmarshal(raw, &got); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if got.State != models.StateFailed {
					t.Fatalf("state = %q, want %q", got.State, models.StateFailed)
				}
				return
			}

			resp, raw := doRequest(t, http.MethodPost,
				srv.URL+"/api/v1/shares/data/snapshots/", tc.body)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d: body=%s", resp.StatusCode, tc.wantStatus, raw)
			}
		})
	}
}
