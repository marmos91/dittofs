package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/marmos91/dittofs/pkg/blockstore/engine"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
)

// fakeGCRuntime is a recording stand-in for handlers.BlockGCRuntime.
// Tests assert on captured arguments and feed canned responses.
type fakeGCRuntime struct {
	// RunBlockGCForShare hooks
	runStats *engine.GCStats
	runErr   error
	runCalls []runCall

	// GCStateDirForShare hooks
	gcStateRoot   string
	gcStateRootEr error
}

type runCall struct {
	share  string
	dryRun bool
}

func (f *fakeGCRuntime) RunBlockGCForShare(_ context.Context, shareName string, dryRun bool) (*engine.GCStats, error) {
	f.runCalls = append(f.runCalls, runCall{share: shareName, dryRun: dryRun})
	if f.runErr != nil {
		return nil, f.runErr
	}
	return f.runStats, nil
}

func (f *fakeGCRuntime) GCStateDirForShare(_ string) (string, error) {
	if f.gcStateRootEr != nil {
		return "", f.gcStateRootEr
	}
	return f.gcStateRoot, nil
}

// newGCRequest builds a chi-aware httptest request with the {name} URL
// param pre-populated. Mirrors newChiRequestForBlockStore but fixed to
// the {name} key the GC handler reads.
func newGCRequest(method, path, share string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, path, body)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", share)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

// TestBlockStoreHandler_RunGC_Success_NotDryRun asserts a non-dry-run
// POST invokes RunBlockGCForShare with dryRun=false and returns the
// captured stats as JSON.
func TestBlockStoreHandler_RunGC_Success_NotDryRun(t *testing.T) {
	fake := &fakeGCRuntime{
		runStats: &engine.GCStats{
			HashesMarked: 42,
			ObjectsSwept: 7,
			BytesFreed:   1024,
		},
	}
	h := NewBlockStoreGCHandler(fake)

	body, _ := json.Marshal(BlockStoreGCRequest{DryRun: false})
	req := newGCRequest(http.MethodPost, "/api/v1/shares/myshare/blockstore/gc", "myshare", bytes.NewReader(body))
	w := httptest.NewRecorder()

	h.RunGC(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("RunGC: expected 200, got %d (body=%q)", w.Code, w.Body.String())
	}
	if len(fake.runCalls) != 1 {
		t.Fatalf("RunGC: expected 1 RunBlockGCForShare call, got %d", len(fake.runCalls))
	}
	if fake.runCalls[0].share != "myshare" {
		t.Fatalf("RunGC: expected share=myshare, got %q", fake.runCalls[0].share)
	}
	if fake.runCalls[0].dryRun {
		t.Fatal("RunGC: expected dryRun=false")
	}

	var resp BlockStoreGCResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("RunGC: decode response: %v", err)
	}
	if resp.Stats == nil || resp.Stats.HashesMarked != 42 || resp.Stats.ObjectsSwept != 7 || resp.Stats.BytesFreed != 1024 {
		t.Fatalf("RunGC: unexpected stats: %+v", resp.Stats)
	}
}

// TestBlockStoreHandler_RunGC_DryRunPropagates asserts dry_run=true
// reaches the runtime and DryRunCandidates round-trip in the response.
func TestBlockStoreHandler_RunGC_DryRunPropagates(t *testing.T) {
	fake := &fakeGCRuntime{
		runStats: &engine.GCStats{
			DryRun:           true,
			HashesMarked:     100,
			DryRunCandidates: []string{"cas/aa/bb/abcdef", "cas/aa/cc/123456"},
		},
	}
	h := NewBlockStoreGCHandler(fake)

	body, _ := json.Marshal(BlockStoreGCRequest{DryRun: true})
	req := newGCRequest(http.MethodPost, "/api/v1/shares/myshare/blockstore/gc", "myshare", bytes.NewReader(body))
	w := httptest.NewRecorder()

	h.RunGC(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("RunGC: expected 200, got %d", w.Code)
	}
	if len(fake.runCalls) != 1 || !fake.runCalls[0].dryRun {
		t.Fatalf("RunGC: expected single call with dryRun=true; got %+v", fake.runCalls)
	}

	var resp BlockStoreGCResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("RunGC: decode response: %v", err)
	}
	if !resp.Stats.DryRun {
		t.Fatal("RunGC: expected DryRun=true in response")
	}
	if len(resp.Stats.DryRunCandidates) != 2 {
		t.Fatalf("RunGC: expected 2 DryRunCandidates, got %d", len(resp.Stats.DryRunCandidates))
	}
}

// TestBlockStoreHandler_RunGC_EmptyBody treats a missing body as the
// zero value (DryRun=false). Operators commonly POST without a body.
func TestBlockStoreHandler_RunGC_EmptyBody(t *testing.T) {
	fake := &fakeGCRuntime{runStats: &engine.GCStats{}}
	h := NewBlockStoreGCHandler(fake)

	req := newGCRequest(http.MethodPost, "/api/v1/shares/myshare/blockstore/gc", "myshare", nil)
	w := httptest.NewRecorder()

	h.RunGC(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("RunGC: expected 200, got %d", w.Code)
	}
	if len(fake.runCalls) != 1 || fake.runCalls[0].dryRun {
		t.Fatalf("RunGC: expected single call with dryRun=false; got %+v", fake.runCalls)
	}
}

// TestBlockStoreHandler_RunGC_MalformedBody returns 400 on bad JSON,
// matching evict's behavior — the request never reaches RunBlockGCForShare.
func TestBlockStoreHandler_RunGC_MalformedBody(t *testing.T) {
	fake := &fakeGCRuntime{runStats: &engine.GCStats{}}
	h := NewBlockStoreGCHandler(fake)

	req := newGCRequest(http.MethodPost, "/api/v1/shares/myshare/blockstore/gc", "myshare", bytes.NewReader([]byte("{not-json")))
	w := httptest.NewRecorder()

	h.RunGC(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("RunGC: expected 400 on malformed body, got %d (body=%q)", w.Code, w.Body.String())
	}
	if len(fake.runCalls) != 0 {
		t.Fatalf("RunGC: runtime must not be invoked on bad input; got %d calls", len(fake.runCalls))
	}
}

// TestBlockStoreHandler_RunGC_ShareNotFound returns 404 when the share
// is unknown. Mirrors models.ErrShareNotFound mapping in MapStoreError
// but goes through shares.ErrShareNotFound for runtime-layer errors.
func TestBlockStoreHandler_RunGC_ShareNotFound(t *testing.T) {
	fake := &fakeGCRuntime{
		runErr: fmt.Errorf("%w: %q", shares.ErrShareNotFound, "ghost"),
	}
	h := NewBlockStoreGCHandler(fake)

	body, _ := json.Marshal(BlockStoreGCRequest{})
	req := newGCRequest(http.MethodPost, "/api/v1/shares/ghost/blockstore/gc", "ghost", bytes.NewReader(body))
	w := httptest.NewRecorder()

	h.RunGC(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("RunGC: expected 404, got %d (body=%q)", w.Code, w.Body.String())
	}
}

// TestBlockStoreHandler_RunGC_EmptyShareName returns 400 when {name}
// is empty. The chi router would normally not match the empty value,
// but the handler defends against direct invocation in tests/probes.
func TestBlockStoreHandler_RunGC_EmptyShareName(t *testing.T) {
	fake := &fakeGCRuntime{runStats: &engine.GCStats{}}
	h := NewBlockStoreGCHandler(fake)

	req := newGCRequest(http.MethodPost, "/api/v1/shares//blockstore/gc", "", nil)
	w := httptest.NewRecorder()

	h.RunGC(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("RunGC: expected 400 on empty share name, got %d", w.Code)
	}
}

// TestBlockStoreHandler_RunGC_NilRuntime fails closed when wired with
// a nil runtime. Defends against a misconfigured server boot path.
func TestBlockStoreHandler_RunGC_NilRuntime(t *testing.T) {
	h := NewBlockStoreGCHandler(nil)

	req := newGCRequest(http.MethodPost, "/api/v1/shares/myshare/blockstore/gc", "myshare", nil)
	w := httptest.NewRecorder()

	h.RunGC(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("RunGC: expected 500 on nil runtime, got %d", w.Code)
	}
}

// TestBlockStoreHandler_GCStatus_Success reads a valid last-run.json
// from the share's gc-state directory and round-trips the parsed
// GCRunSummary as JSON.
func TestBlockStoreHandler_GCStatus_Success(t *testing.T) {
	root := t.TempDir()
	// Pre-seed last-run.json so the handler can read it.
	summary := engine.GCRunSummary{
		RunID:        "test-run-1",
		StartedAt:    time.Now().UTC().Truncate(time.Second),
		CompletedAt:  time.Now().UTC().Truncate(time.Second).Add(time.Second),
		HashesMarked: 17,
		ObjectsSwept: 3,
		BytesFreed:   2048,
		DurationMs:   1000,
	}
	if err := engine.PersistLastRunSummary(root, summary); err != nil {
		t.Fatalf("seed last-run.json: %v", err)
	}

	fake := &fakeGCRuntime{gcStateRoot: root}
	h := NewBlockStoreGCHandler(fake)

	req := newGCRequest(http.MethodGet, "/api/v1/shares/myshare/blockstore/gc-status", "myshare", nil)
	w := httptest.NewRecorder()

	h.GCStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GCStatus: expected 200, got %d (body=%q)", w.Code, w.Body.String())
	}

	var got engine.GCRunSummary
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("GCStatus: decode response: %v", err)
	}
	if got.RunID != summary.RunID || got.HashesMarked != summary.HashesMarked || got.BytesFreed != summary.BytesFreed {
		t.Fatalf("GCStatus: round-trip mismatch: got %+v want %+v", got, summary)
	}
}

// TestBlockStoreHandler_GCStatus_NoRunYet returns 404 when the
// last-run.json file does not exist (the share has never run GC).
func TestBlockStoreHandler_GCStatus_NoRunYet(t *testing.T) {
	root := t.TempDir() // empty: no last-run.json

	fake := &fakeGCRuntime{gcStateRoot: root}
	h := NewBlockStoreGCHandler(fake)

	req := newGCRequest(http.MethodGet, "/api/v1/shares/myshare/blockstore/gc-status", "myshare", nil)
	w := httptest.NewRecorder()

	h.GCStatus(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("GCStatus: expected 404 when no last-run.json, got %d", w.Code)
	}
}

// TestBlockStoreHandler_GCStatus_EmptyRoot returns 404 when the share's
// local store has no persistent root (in-memory backend).
func TestBlockStoreHandler_GCStatus_EmptyRoot(t *testing.T) {
	fake := &fakeGCRuntime{gcStateRoot: ""}
	h := NewBlockStoreGCHandler(fake)

	req := newGCRequest(http.MethodGet, "/api/v1/shares/myshare/blockstore/gc-status", "myshare", nil)
	w := httptest.NewRecorder()

	h.GCStatus(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("GCStatus: expected 404 when gcStateRoot empty, got %d", w.Code)
	}
}

// TestBlockStoreHandler_GCStatus_ShareNotFound returns 404 when the
// share is unknown (GCStateDirForShare wraps shares.ErrShareNotFound).
func TestBlockStoreHandler_GCStatus_ShareNotFound(t *testing.T) {
	fake := &fakeGCRuntime{
		gcStateRootEr: fmt.Errorf("%w: %q", shares.ErrShareNotFound, "ghost"),
	}
	h := NewBlockStoreGCHandler(fake)

	req := newGCRequest(http.MethodGet, "/api/v1/shares/ghost/blockstore/gc-status", "ghost", nil)
	w := httptest.NewRecorder()

	h.GCStatus(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("GCStatus: expected 404 when share missing, got %d", w.Code)
	}
}

// TestBlockStoreHandler_GCStatus_MalformedFile returns 500 when
// last-run.json exists but cannot be parsed (corrupt or truncated).
func TestBlockStoreHandler_GCStatus_MalformedFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "last-run.json"), []byte("{not-json"), 0o644); err != nil {
		t.Fatalf("seed corrupt last-run.json: %v", err)
	}

	fake := &fakeGCRuntime{gcStateRoot: root}
	h := NewBlockStoreGCHandler(fake)

	req := newGCRequest(http.MethodGet, "/api/v1/shares/myshare/blockstore/gc-status", "myshare", nil)
	w := httptest.NewRecorder()

	h.GCStatus(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("GCStatus: expected 500 on parse failure, got %d", w.Code)
	}
}

// TestBlockStoreHandler_GCStatus_NilRuntime fails closed when wired
// with a nil runtime.
func TestBlockStoreHandler_GCStatus_NilRuntime(t *testing.T) {
	h := NewBlockStoreGCHandler(nil)

	req := newGCRequest(http.MethodGet, "/api/v1/shares/myshare/blockstore/gc-status", "myshare", nil)
	w := httptest.NewRecorder()

	h.GCStatus(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("GCStatus: expected 500 on nil runtime, got %d", w.Code)
	}
}
