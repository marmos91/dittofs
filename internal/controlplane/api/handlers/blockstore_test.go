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

func TestBlockStoreHandler_Evict_NoBody(t *testing.T) {
	th := newTestBlockStoreHandler()

	req := newChiRequestForBlockStore(http.MethodPost, "/api/v1/blockstore/evict", nil)
	w := httptest.NewRecorder()

	th.Evict(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Evict() status = %d, want %d, body = %s", w.Code, http.StatusOK, w.Body.String())
	}
}
