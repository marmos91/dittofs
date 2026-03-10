package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/marmos91/dittofs/pkg/blockstore/engine"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
)

// mockCacheRuntime implements the subset of runtime methods needed for cache tests.
type mockCacheRuntime struct {
	stats     *shares.CacheStatsResponse
	statsErr  error
	evict     *shares.EvictResult
	evictErr  error
	lastShare string
	lastOpts  shares.EvictOptions
}

// testCacheHandler wraps CacheHandler with a mock runtime for testing.
type testCacheHandler struct {
	mock    *mockCacheRuntime
	handler *testCacheHandlerShim
}

// testCacheHandlerShim provides the same interface as CacheHandler but uses a mock.
type testCacheHandlerShim struct {
	mock *mockCacheRuntime
}

func (h *testCacheHandlerShim) Stats(w http.ResponseWriter, r *http.Request) {
	shareName := chi.URLParam(r, "name")
	h.mock.lastShare = shareName

	if h.mock.statsErr != nil {
		NotFound(w, h.mock.statsErr.Error())
		return
	}

	WriteJSONOK(w, h.mock.stats)
}

func (h *testCacheHandlerShim) Evict(w http.ResponseWriter, r *http.Request) {
	shareName := chi.URLParam(r, "name")
	h.mock.lastShare = shareName

	var req CacheEvictRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			BadRequest(w, "invalid request body: "+err.Error())
			return
		}
	}
	h.mock.lastOpts = shares.EvictOptions{
		L1Only:    req.L1Only,
		LocalOnly: req.LocalOnly,
	}

	if h.mock.evictErr != nil {
		BadRequest(w, h.mock.evictErr.Error())
		return
	}

	WriteJSONOK(w, h.mock.evict)
}

func newTestCacheHandler() *testCacheHandler {
	mock := &mockCacheRuntime{
		stats: &shares.CacheStatsResponse{
			Totals: engine.CacheStats{
				FileCount:      5,
				BlocksTotal:    20,
				LocalDiskUsed:  1024 * 1024,
				L1Entries:      10,
				HasRemote:      true,
				PendingSyncs:   2,
				PendingUploads: 1,
			},
			PerShare: []shares.ShareCacheStats{
				{
					ShareName: "/test",
					Stats: engine.CacheStats{
						FileCount:   5,
						BlocksTotal: 20,
						HasRemote:   true,
					},
				},
			},
		},
		evict: &shares.EvictResult{
			L1EntriesCleared:   10,
			LocalBlocksEvicted: 3,
			BytesFreed:         1024 * 1024,
		},
	}
	return &testCacheHandler{
		mock:    mock,
		handler: &testCacheHandlerShim{mock: mock},
	}
}

func TestCacheHandler_Stats_Global(t *testing.T) {
	th := newTestCacheHandler()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/cache/stats", nil)
	rctx := chi.NewRouteContext()
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()

	th.handler.Stats(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Stats() status = %d, want %d, body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp shares.CacheStatsResponse
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

func TestCacheHandler_Stats_PerShare(t *testing.T) {
	th := newTestCacheHandler()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/shares/test/cache/stats", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", "/test")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()

	th.handler.Stats(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Stats() status = %d, want %d", w.Code, http.StatusOK)
	}
	if th.mock.lastShare != "/test" {
		t.Errorf("lastShare = %q, want %q", th.mock.lastShare, "/test")
	}
}

func TestCacheHandler_Stats_NotFound(t *testing.T) {
	th := newTestCacheHandler()
	th.mock.statsErr = &testError{msg: "share not found"}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/shares/missing/cache/stats", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", "/missing")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()

	th.handler.Stats(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("Stats() status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestCacheHandler_Evict_Global(t *testing.T) {
	th := newTestCacheHandler()

	body, _ := json.Marshal(CacheEvictRequest{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cache/evict", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rctx := chi.NewRouteContext()
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()

	th.handler.Evict(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Evict() status = %d, want %d, body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp shares.EvictResult
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}
	if resp.L1EntriesCleared != 10 {
		t.Errorf("L1EntriesCleared = %d, want 10", resp.L1EntriesCleared)
	}
	if resp.LocalBlocksEvicted != 3 {
		t.Errorf("LocalBlocksEvicted = %d, want 3", resp.LocalBlocksEvicted)
	}
}

func TestCacheHandler_Evict_L1Only(t *testing.T) {
	th := newTestCacheHandler()

	body, _ := json.Marshal(CacheEvictRequest{L1Only: true})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cache/evict", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rctx := chi.NewRouteContext()
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()

	th.handler.Evict(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Evict() status = %d, want %d", w.Code, http.StatusOK)
	}
	if !th.mock.lastOpts.L1Only {
		t.Error("Expected L1Only=true in opts")
	}
}

func TestCacheHandler_Evict_SafetyError(t *testing.T) {
	th := newTestCacheHandler()
	th.mock.evictErr = &testError{msg: "cannot evict local blocks: no remote store configured"}

	body, _ := json.Marshal(CacheEvictRequest{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cache/evict", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rctx := chi.NewRouteContext()
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()

	th.handler.Evict(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Evict() status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestCacheHandler_Evict_NoBody(t *testing.T) {
	th := newTestCacheHandler()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/cache/evict", nil)
	rctx := chi.NewRouteContext()
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()

	th.handler.Evict(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Evict() status = %d, want %d, body = %s", w.Code, http.StatusOK, w.Body.String())
	}
}

// testError implements the error interface for testing.
type testError struct {
	msg string
}

func (e *testError) Error() string {
	return e.msg
}
