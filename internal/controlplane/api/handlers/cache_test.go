package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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

// testCacheHandler provides the same interface as CacheHandler but uses a mock.
type testCacheHandler struct {
	mock *mockCacheRuntime
}

func (h *testCacheHandler) Stats(w http.ResponseWriter, r *http.Request) {
	shareName := chi.URLParam(r, "name")
	h.mock.lastShare = shareName

	if h.mock.statsErr != nil {
		NotFound(w, h.mock.statsErr.Error())
		return
	}

	WriteJSONOK(w, h.mock.stats)
}

func (h *testCacheHandler) Evict(w http.ResponseWriter, r *http.Request) {
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
	return &testCacheHandler{mock: mock}
}

// newChiRequest creates an httptest.Request with a chi route context.
// If params is non-empty, they are added as URL params (alternating key/value pairs).
func newChiRequest(method, url string, body io.Reader, params ...string) *http.Request {
	req := httptest.NewRequest(method, url, body)
	rctx := chi.NewRouteContext()
	for i := 0; i+1 < len(params); i += 2 {
		rctx.URLParams.Add(params[i], params[i+1])
	}
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

func TestCacheHandler_Stats_Global(t *testing.T) {
	th := newTestCacheHandler()

	req := newChiRequest(http.MethodGet, "/api/v1/cache/stats", nil)
	w := httptest.NewRecorder()

	th.Stats(w, req)

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

	req := newChiRequest(http.MethodGet, "/api/v1/shares/test/cache/stats", nil, "name", "/test")
	w := httptest.NewRecorder()

	th.Stats(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Stats() status = %d, want %d", w.Code, http.StatusOK)
	}
	if th.mock.lastShare != "/test" {
		t.Errorf("lastShare = %q, want %q", th.mock.lastShare, "/test")
	}
}

func TestCacheHandler_Stats_NotFound(t *testing.T) {
	th := newTestCacheHandler()
	th.mock.statsErr = errors.New("share not found")

	req := newChiRequest(http.MethodGet, "/api/v1/shares/missing/cache/stats", nil, "name", "/missing")
	w := httptest.NewRecorder()

	th.Stats(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("Stats() status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestCacheHandler_Evict_Global(t *testing.T) {
	th := newTestCacheHandler()

	body, _ := json.Marshal(CacheEvictRequest{})
	req := newChiRequest(http.MethodPost, "/api/v1/cache/evict", bytes.NewReader(body))
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
	req := newChiRequest(http.MethodPost, "/api/v1/cache/evict", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	th.Evict(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Evict() status = %d, want %d", w.Code, http.StatusOK)
	}
	if !th.mock.lastOpts.L1Only {
		t.Error("Expected L1Only=true in opts")
	}
}

func TestCacheHandler_Evict_SafetyError(t *testing.T) {
	th := newTestCacheHandler()
	th.mock.evictErr = errors.New("cannot evict local blocks: no remote store configured")

	body, _ := json.Marshal(CacheEvictRequest{})
	req := newChiRequest(http.MethodPost, "/api/v1/cache/evict", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	th.Evict(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Evict() status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestCacheHandler_Evict_NoBody(t *testing.T) {
	th := newTestCacheHandler()

	req := newChiRequest(http.MethodPost, "/api/v1/cache/evict", nil)
	w := httptest.NewRecorder()

	th.Evict(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Evict() status = %d, want %d, body = %s", w.Code, http.StatusOK, w.Body.String())
	}
}
