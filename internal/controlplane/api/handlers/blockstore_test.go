package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
)

// mockBlockStoreRuntime implements the subset of runtime methods needed for block store tests.
type mockBlockStoreRuntime struct {
	stats     *shares.BlockStoreStatsResponse
	statsErr  error
	evict     *shares.EvictResult
	evictErr  error
	lastShare string
	lastOpts  shares.EvictOptions
}

// testBlockStoreHandler provides the same interface as BlockStoreHandler but uses a mock.
type testBlockStoreHandler struct {
	mock *mockBlockStoreRuntime
}

func (h *testBlockStoreHandler) Stats(w http.ResponseWriter, r *http.Request) {
	shareName := chi.URLParam(r, "name")
	h.mock.lastShare = shareName

	if h.mock.statsErr != nil {
		NotFound(w, h.mock.statsErr.Error())
		return
	}

	WriteJSONOK(w, h.mock.stats)
}

func (h *testBlockStoreHandler) Evict(w http.ResponseWriter, r *http.Request) {
	shareName := chi.URLParam(r, "name")
	h.mock.lastShare = shareName

	var req BlockStoreEvictRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			BadRequest(w, "invalid request body: "+err.Error())
			return
		}
	}
	h.mock.lastOpts = shares.EvictOptions{
		ReadBufferOnly: req.ReadBufferOnly,
		LocalOnly:      req.LocalOnly,
	}

	if h.mock.evictErr != nil {
		BadRequest(w, h.mock.evictErr.Error())
		return
	}

	WriteJSONOK(w, h.mock.evict)
}

func newTestBlockStoreHandler() *testBlockStoreHandler {
	mock := &mockBlockStoreRuntime{
		stats: &shares.BlockStoreStatsResponse{
			Totals: engine.BlockStoreStats{
				FileCount:         5,
				BlocksTotal:       20,
				LocalDiskUsed:     1024 * 1024,
				ReadBufferEntries: 10,
				HasRemote:         true,
				PendingSyncs:      2,
				PendingUploads:    1,
			},
			PerShare: []shares.ShareBlockStoreStats{
				{
					ShareName: "/test",
					Stats: engine.BlockStoreStats{
						FileCount:   5,
						BlocksTotal: 20,
						HasRemote:   true,
					},
				},
			},
		},
		evict: &shares.EvictResult{
			ReadBufferEntriesCleared: 10,
			LocalFilesEvicted:        3,
			BytesFreed:               1024 * 1024,
		},
	}
	return &testBlockStoreHandler{mock: mock}
}

// newChiRequestForBlockStore creates an httptest.Request with a chi route context.
// If params is non-empty, they are added as URL params (alternating key/value pairs).
func newChiRequestForBlockStore(method, url string, body io.Reader, params ...string) *http.Request {
	req := httptest.NewRequest(method, url, body)
	rctx := chi.NewRouteContext()
	for i := 0; i+1 < len(params); i += 2 {
		rctx.URLParams.Add(params[i], params[i+1])
	}
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

func TestBlockStoreHandler_Stats_Global(t *testing.T) {
	th := newTestBlockStoreHandler()

	req := newChiRequestForBlockStore(http.MethodGet, "/api/v1/blockstore/stats", nil)
	w := httptest.NewRecorder()

	th.Stats(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Stats() status = %d, want %d, body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp shares.BlockStoreStatsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}
	if resp.Totals.FileCount != 5 {
		t.Errorf("FileCount = %d, want 5", resp.Totals.FileCount)
	}
	if resp.Totals.BlocksTotal != 20 {
		t.Errorf("BlocksTotal = %d, want 20", resp.Totals.BlocksTotal)
	}
	if len(resp.PerShare) != 1 {
		t.Errorf("PerShare len = %d, want 1", len(resp.PerShare))
	}
}

func TestBlockStoreHandler_Stats_PerShare(t *testing.T) {
	th := newTestBlockStoreHandler()

	req := newChiRequestForBlockStore(http.MethodGet, "/api/v1/shares/test/blockstore/stats", nil, "name", "/test")
	w := httptest.NewRecorder()

	th.Stats(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Stats() status = %d, want %d", w.Code, http.StatusOK)
	}
	if th.mock.lastShare != "/test" {
		t.Errorf("lastShare = %q, want %q", th.mock.lastShare, "/test")
	}
}

func TestBlockStoreHandler_Stats_NotFound(t *testing.T) {
	th := newTestBlockStoreHandler()
	th.mock.statsErr = errors.New("share not found")

	req := newChiRequestForBlockStore(http.MethodGet, "/api/v1/shares/missing/blockstore/stats", nil, "name", "/missing")
	w := httptest.NewRecorder()

	th.Stats(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("Stats() status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestBlockStoreHandler_Evict_Global(t *testing.T) {
	th := newTestBlockStoreHandler()

	body, _ := json.Marshal(BlockStoreEvictRequest{})
	req := newChiRequestForBlockStore(http.MethodPost, "/api/v1/blockstore/evict", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	th.Evict(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Evict() status = %d, want %d, body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp shares.EvictResult
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}
	if resp.ReadBufferEntriesCleared != 10 {
		t.Errorf("ReadBufferEntriesCleared = %d, want 10", resp.ReadBufferEntriesCleared)
	}
	if resp.LocalFilesEvicted != 3 {
		t.Errorf("LocalFilesEvicted = %d, want 3", resp.LocalFilesEvicted)
	}
}

func TestBlockStoreHandler_Evict_ReadBufferOnly(t *testing.T) {
	th := newTestBlockStoreHandler()

	body, _ := json.Marshal(BlockStoreEvictRequest{ReadBufferOnly: true})
	req := newChiRequestForBlockStore(http.MethodPost, "/api/v1/blockstore/evict", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	th.Evict(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Evict() status = %d, want %d", w.Code, http.StatusOK)
	}
	if !th.mock.lastOpts.ReadBufferOnly {
		t.Error("Expected ReadBufferOnly=true in opts")
	}
}

func TestBlockStoreHandler_Evict_SafetyError(t *testing.T) {
	th := newTestBlockStoreHandler()
	th.mock.evictErr = errors.New("cannot evict local blocks: no remote store configured")

	body, _ := json.Marshal(BlockStoreEvictRequest{})
	req := newChiRequestForBlockStore(http.MethodPost, "/api/v1/blockstore/evict", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	th.Evict(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Evict() status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// fakeBlockStoreRuntime implements BlockStoreRuntime for exercising the real
// BlockStoreStatsHandler (as opposed to the testBlockStoreHandler shim above).
type fakeBlockStoreRuntime struct {
	stats      *shares.BlockStoreStatsResponse
	statsErr   error
	evict      *shares.EvictResult
	evictErr   error
	lastShare  string
	warmJob    *shares.WarmJob
	warmErr    error
	warmStatus *shares.WarmJob
	warmFound  bool
}

func (f *fakeBlockStoreRuntime) GetBlockStoreStats(shareName string) (*shares.BlockStoreStatsResponse, error) {
	f.lastShare = shareName
	return f.stats, f.statsErr
}

func (f *fakeBlockStoreRuntime) EvictBlockStore(_ context.Context, shareName string, _ shares.EvictOptions) (*shares.EvictResult, error) {
	f.lastShare = shareName
	return f.evict, f.evictErr
}

func (f *fakeBlockStoreRuntime) StartWarmBlockStore(_ context.Context, shareName string) (*shares.WarmJob, error) {
	f.lastShare = shareName
	return f.warmJob, f.warmErr
}

func (f *fakeBlockStoreRuntime) GetWarmStatus(_ string) (*shares.WarmJob, bool) {
	return f.warmStatus, f.warmFound
}

// TestBlockStoreStatsHandler_NormalizesShareName verifies the real handler
// prepends the registry's leading slash for a bare per-share URL param while
// leaving the global route (no {name}) as the empty "all shares" key.
func TestBlockStoreStatsHandler_NormalizesShareName(t *testing.T) {
	t.Run("per_share_normalized", func(t *testing.T) {
		fake := &fakeBlockStoreRuntime{stats: &shares.BlockStoreStatsResponse{}}
		h := NewBlockStoreStatsHandler(fake)
		req := newChiRequestForBlockStore(http.MethodGet,
			"/api/v1/shares/myshare/blockstore/stats", nil, "name", "myshare")
		h.Stats(httptest.NewRecorder(), req)
		if fake.lastShare != "/myshare" {
			t.Fatalf("Stats: runtime got %q, want /myshare", fake.lastShare)
		}
	})

	t.Run("global_stays_empty", func(t *testing.T) {
		fake := &fakeBlockStoreRuntime{stats: &shares.BlockStoreStatsResponse{}}
		h := NewBlockStoreStatsHandler(fake)
		req := newChiRequestForBlockStore(http.MethodGet, "/api/v1/blockstore/stats", nil)
		h.Stats(httptest.NewRecorder(), req)
		if fake.lastShare != "" {
			t.Fatalf("Stats (global): runtime got %q, want empty (all shares)", fake.lastShare)
		}
	})

	t.Run("evict_per_share_normalized", func(t *testing.T) {
		fake := &fakeBlockStoreRuntime{evict: &shares.EvictResult{}}
		h := NewBlockStoreStatsHandler(fake)
		body, _ := json.Marshal(BlockStoreEvictRequest{})
		req := newChiRequestForBlockStore(http.MethodPost,
			"/api/v1/shares/myshare/blockstore/evict", bytes.NewReader(body), "name", "myshare")
		h.Evict(httptest.NewRecorder(), req)
		if fake.lastShare != "/myshare" {
			t.Fatalf("Evict: runtime got %q, want /myshare", fake.lastShare)
		}
	})

	t.Run("evict_global_stays_empty", func(t *testing.T) {
		fake := &fakeBlockStoreRuntime{evict: &shares.EvictResult{}}
		h := NewBlockStoreStatsHandler(fake)
		body, _ := json.Marshal(BlockStoreEvictRequest{})
		req := newChiRequestForBlockStore(http.MethodPost,
			"/api/v1/blockstore/evict", bytes.NewReader(body))
		h.Evict(httptest.NewRecorder(), req)
		if fake.lastShare != "" {
			t.Fatalf("Evict (global): runtime got %q, want empty (all shares)", fake.lastShare)
		}
	})
}

// TestBlockStoreStatsHandler_Warm covers the async warm start (202 + job id)
// and the status poll, including share-name normalization and the 404 for an
// unknown job.
func TestBlockStoreStatsHandler_Warm(t *testing.T) {
	t.Run("start_returns_202_and_job_id", func(t *testing.T) {
		fake := &fakeBlockStoreRuntime{warmJob: &shares.WarmJob{ID: "warm-1", Share: "/myshare", State: shares.WarmStateRunning}}
		h := NewBlockStoreStatsHandler(fake)
		req := newChiRequestForBlockStore(http.MethodPost,
			"/api/v1/shares/myshare/blockstore/warm", nil, "name", "myshare")
		w := httptest.NewRecorder()
		h.Warm(w, req)

		if w.Code != http.StatusAccepted {
			t.Fatalf("Warm status = %d, want 202", w.Code)
		}
		if fake.lastShare != "/myshare" {
			t.Fatalf("Warm: runtime got %q, want /myshare", fake.lastShare)
		}
		var body struct {
			JobID string `json:"job_id"`
		}
		if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.JobID != "warm-1" {
			t.Fatalf("job_id = %q, want warm-1", body.JobID)
		}
	})

	t.Run("start_error_not_leaked", func(t *testing.T) {
		fake := &fakeBlockStoreRuntime{warmErr: errors.New(`share "/secret" has no remote tier`)}
		h := NewBlockStoreStatsHandler(fake)
		req := newChiRequestForBlockStore(http.MethodPost,
			"/api/v1/shares/secret/blockstore/warm", nil, "name", "secret")
		w := httptest.NewRecorder()
		h.Warm(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("Warm error status = %d, want 400", w.Code)
		}
		var p Problem
		_ = json.NewDecoder(w.Body).Decode(&p)
		if strings.Contains(p.Detail, "secret") {
			t.Errorf("Detail leaks share name: %q", p.Detail)
		}
	})

	t.Run("status_found", func(t *testing.T) {
		fake := &fakeBlockStoreRuntime{
			warmStatus: &shares.WarmJob{ID: "warm-1", Share: "/myshare", State: shares.WarmStateDone, BlocksTotal: 5, BlocksDone: 5, BytesDone: 100, Warning: "block metadata may be missing"},
			warmFound:  true,
		}
		h := NewBlockStoreStatsHandler(fake)
		req := newChiRequestForBlockStore(http.MethodGet,
			"/api/v1/shares/myshare/blockstore/warm/warm-1", nil, "name", "myshare", "job_id", "warm-1")
		w := httptest.NewRecorder()
		h.WarmStatus(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("WarmStatus = %d, want 200", w.Code)
		}
		var resp WarmJobStatusResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Warning != "block metadata may be missing" {
			t.Fatalf("WarmStatus warning = %q, want passthrough", resp.Warning)
		}
		if resp.State != shares.WarmStateDone || resp.BlocksDone != 5 || resp.BytesDone != 100 {
			t.Fatalf("unexpected status: %+v", resp)
		}
	})

	t.Run("status_unknown_404", func(t *testing.T) {
		fake := &fakeBlockStoreRuntime{warmFound: false}
		h := NewBlockStoreStatsHandler(fake)
		req := newChiRequestForBlockStore(http.MethodGet,
			"/api/v1/shares/myshare/blockstore/warm/nope", nil, "name", "myshare", "job_id", "nope")
		w := httptest.NewRecorder()
		h.WarmStatus(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("WarmStatus unknown = %d, want 404", w.Code)
		}
	})
}

// TestBlockStoreStatsHandler_Stats_ErrorDetailNotLeaked asserts that when
// GetBlockStoreStats returns an error the handler writes a static 404 body
// rather than leaking the internal error string.
func TestBlockStoreStatsHandler_Stats_ErrorDetailNotLeaked(t *testing.T) {
	internalMsg := `share "/secret-share" not found`
	fake := &fakeBlockStoreRuntime{statsErr: errors.New(internalMsg)}
	h := NewBlockStoreStatsHandler(fake)

	req := newChiRequestForBlockStore(http.MethodGet,
		"/api/v1/shares/secret-share/blockstore/stats", nil, "name", "/secret-share")
	w := httptest.NewRecorder()
	h.Stats(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
	var p Problem
	if err := json.NewDecoder(w.Body).Decode(&p); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if strings.Contains(p.Detail, "secret-share") {
		t.Errorf("Detail leaks internal share name: %q", p.Detail)
	}
	if p.Detail != "share not found" {
		t.Errorf("Detail = %q, want %q", p.Detail, "share not found")
	}
}

// TestBlockStoreStatsHandler_Evict_ErrorDetailNotLeaked asserts that when
// EvictBlockStore returns an error the handler writes a static 400 body
// rather than leaking internal storage topology details.
func TestBlockStoreStatsHandler_Evict_ErrorDetailNotLeaked(t *testing.T) {
	internalMsg := `cannot evict local blocks for share "/secret-share": no remote store configured (data would be lost)`
	fake := &fakeBlockStoreRuntime{evictErr: errors.New(internalMsg)}
	h := NewBlockStoreStatsHandler(fake)

	body, _ := json.Marshal(BlockStoreEvictRequest{})
	req := newChiRequestForBlockStore(http.MethodPost,
		"/api/v1/shares/secret-share/blockstore/evict",
		bytes.NewReader(body), "name", "/secret-share")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.Evict(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	var p Problem
	if err := json.NewDecoder(w.Body).Decode(&p); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if strings.Contains(p.Detail, "secret-share") || strings.Contains(p.Detail, "remote store") {
		t.Errorf("Detail leaks internal detail: %q", p.Detail)
	}
	if p.Detail != "eviction failed" {
		t.Errorf("Detail = %q, want %q", p.Detail, "eviction failed")
	}
}

func TestBlockStoreHandler_Evict_NoBody(t *testing.T) {
	th := newTestBlockStoreHandler()

	req := newChiRequestForBlockStore(http.MethodPost, "/api/v1/blockstore/evict", nil)
	w := httptest.NewRecorder()

	th.Evict(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Evict() status = %d, want %d, body = %s", w.Code, http.StatusOK, w.Body.String())
	}
}
