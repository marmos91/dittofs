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

	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
)

// fakeGCRuntime is a recording stand-in for handlers.BlockGCRuntime.
// Tests assert on captured arguments and feed canned responses.
type fakeGCRuntime struct {
	// StartBlockGC hooks
	startJob   *runtime.GCJob
	startErr   error
	startCalls []startCall

	// GetGCJobStatus hooks
	statusJob *runtime.GCJob
	statusOK  bool

	// GCStateDirForShare hooks
	gcStateRoot   string
	gcStateRootEr error
}

type startCall struct {
	share       string
	dryRun      bool
	reconcile   bool
	gracePeriod *time.Duration
}

func (f *fakeGCRuntime) StartBlockGC(shareName string, dryRun, reconcile bool, gracePeriod *time.Duration) (*runtime.GCJob, error) {
	f.startCalls = append(f.startCalls, startCall{share: shareName, dryRun: dryRun, reconcile: reconcile, gracePeriod: gracePeriod})
	if f.startErr != nil {
		return nil, f.startErr
	}
	if f.startJob != nil {
		return f.startJob, nil
	}
	return &runtime.GCJob{ID: "gc-1", State: runtime.GCStateRunning, Share: shareName, DryRun: dryRun, Reconcile: reconcile}, nil
}

func (f *fakeGCRuntime) GetGCJobStatus(string) (*runtime.GCJob, bool) {
	return f.statusJob, f.statusOK
}

func (f *fakeGCRuntime) GCStateDirForShare(_ string) (string, error) {
	if f.gcStateRootEr != nil {
		return "", f.gcStateRootEr
	}
	return f.gcStateRoot, nil
}

// gcStartBody mirrors the 202 response shape from RunGC.
type gcStartBody struct {
	JobID  string              `json:"job_id"`
	Status GCJobStatusResponse `json:"status"`
}

// newGCRequest builds a chi-aware httptest request with the {name} URL param
// pre-populated.
func newGCRequest(method, path, share string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, path, body)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", share)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

// newGCJobRequest additionally pre-populates the {job_id} param.
func newGCJobRequest(method, path, share, jobID string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, path, body)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", share)
	rctx.URLParams.Add("job_id", jobID)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

// TestBlockStoreHandler_RunGC_KicksOffJob asserts a non-dry-run POST starts an
// async job via StartBlockGC (dryRun=false, reconcile=false) and returns 202
// with the job id + initial status.
func TestBlockStoreHandler_RunGC_KicksOffJob(t *testing.T) {
	fake := &fakeGCRuntime{}
	h := NewBlockStoreGCHandler(fake)

	body, _ := json.Marshal(BlockStoreGCRequest{DryRun: false})
	req := newGCRequest(http.MethodPost, "/api/v1/shares/myshare/blockstore/gc", "myshare", bytes.NewReader(body))
	w := httptest.NewRecorder()

	h.RunGC(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("RunGC: expected 202, got %d (body=%q)", w.Code, w.Body.String())
	}
	if len(fake.startCalls) != 1 {
		t.Fatalf("RunGC: expected 1 StartBlockGC call, got %d", len(fake.startCalls))
	}
	// The handler must normalize the bare URL param ("myshare") to the
	// registry key ("/myshare") before calling the runtime.
	if fake.startCalls[0].share != "/myshare" {
		t.Fatalf("RunGC: expected normalized share=/myshare, got %q", fake.startCalls[0].share)
	}
	if fake.startCalls[0].dryRun || fake.startCalls[0].reconcile {
		t.Fatalf("RunGC: expected dryRun=false reconcile=false, got %+v", fake.startCalls[0])
	}

	var resp gcStartBody
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("RunGC: decode response: %v", err)
	}
	if resp.JobID == "" || resp.Status.State != runtime.GCStateRunning {
		t.Fatalf("RunGC: unexpected start body: %+v", resp)
	}
}

// TestBlockStoreHandler_RunGC_DryRunReconcilePropagate asserts both flags reach
// the runtime.
func TestBlockStoreHandler_RunGC_DryRunReconcilePropagate(t *testing.T) {
	fake := &fakeGCRuntime{}
	h := NewBlockStoreGCHandler(fake)

	body, _ := json.Marshal(BlockStoreGCRequest{DryRun: true, Reconcile: true})
	req := newGCRequest(http.MethodPost, "/api/v1/shares/myshare/blockstore/gc", "myshare", bytes.NewReader(body))
	w := httptest.NewRecorder()

	h.RunGC(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("RunGC: expected 202, got %d", w.Code)
	}
	if len(fake.startCalls) != 1 || !fake.startCalls[0].dryRun || !fake.startCalls[0].reconcile {
		t.Fatalf("RunGC: expected dryRun+reconcile propagated; got %+v", fake.startCalls)
	}
}

// TestBlockStoreHandler_RunGC_GracePeriodOverride asserts a zero grace override
// reaches the runtime as an explicit 0 duration (not nil).
func TestBlockStoreHandler_RunGC_GracePeriodOverride(t *testing.T) {
	fake := &fakeGCRuntime{}
	h := NewBlockStoreGCHandler(fake)

	zero := int64(0)
	body, _ := json.Marshal(BlockStoreGCRequest{GracePeriodSeconds: &zero})
	req := newGCRequest(http.MethodPost, "/api/v1/shares/myshare/blockstore/gc", "myshare", bytes.NewReader(body))
	w := httptest.NewRecorder()

	h.RunGC(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("RunGC: expected 202, got %d (body=%q)", w.Code, w.Body.String())
	}
	if len(fake.startCalls) != 1 {
		t.Fatalf("RunGC: expected 1 StartBlockGC call, got %d", len(fake.startCalls))
	}
	gp := fake.startCalls[0].gracePeriod
	if gp == nil || *gp != 0 {
		t.Fatalf("RunGC: expected gracePeriod==0 override, got %v", gp)
	}
}

// TestBlockStoreHandler_RunGC_GracePeriodRejectsReconcile returns 400 when the
// grace override is combined with reconcile (the override is share-scoped only),
// and never starts a job.
func TestBlockStoreHandler_RunGC_GracePeriodRejectsReconcile(t *testing.T) {
	fake := &fakeGCRuntime{}
	h := NewBlockStoreGCHandler(fake)

	secs := int64(30)
	body, _ := json.Marshal(BlockStoreGCRequest{Reconcile: true, GracePeriodSeconds: &secs})
	req := newGCRequest(http.MethodPost, "/api/v1/shares/myshare/blockstore/gc", "myshare", bytes.NewReader(body))
	w := httptest.NewRecorder()

	h.RunGC(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("RunGC: expected 400 for grace+reconcile, got %d", w.Code)
	}
	if len(fake.startCalls) != 0 {
		t.Fatalf("RunGC: expected no StartBlockGC call on rejected request, got %d", len(fake.startCalls))
	}
}

// TestBlockStoreHandler_RunGC_GracePeriodRejectsNegative returns 400 for a
// negative grace override.
func TestBlockStoreHandler_RunGC_GracePeriodRejectsNegative(t *testing.T) {
	fake := &fakeGCRuntime{}
	h := NewBlockStoreGCHandler(fake)

	neg := int64(-1)
	body, _ := json.Marshal(BlockStoreGCRequest{GracePeriodSeconds: &neg})
	req := newGCRequest(http.MethodPost, "/api/v1/shares/myshare/blockstore/gc", "myshare", bytes.NewReader(body))
	w := httptest.NewRecorder()

	h.RunGC(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("RunGC: expected 400 for negative grace, got %d", w.Code)
	}
	if len(fake.startCalls) != 0 {
		t.Fatalf("RunGC: expected no StartBlockGC call, got %d", len(fake.startCalls))
	}
}

// TestBlockStoreHandler_RunGC_EmptyBody treats a missing body as the zero value
// (DryRun=false).
func TestBlockStoreHandler_RunGC_EmptyBody(t *testing.T) {
	fake := &fakeGCRuntime{}
	h := NewBlockStoreGCHandler(fake)

	req := newGCRequest(http.MethodPost, "/api/v1/shares/myshare/blockstore/gc", "myshare", nil)
	w := httptest.NewRecorder()

	h.RunGC(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("RunGC: expected 202, got %d", w.Code)
	}
	if len(fake.startCalls) != 1 || fake.startCalls[0].dryRun {
		t.Fatalf("RunGC: expected single call with dryRun=false; got %+v", fake.startCalls)
	}
}

// TestBlockStoreHandler_RunGC_MalformedBody returns 400 on bad JSON — the
// request never reaches StartBlockGC.
func TestBlockStoreHandler_RunGC_MalformedBody(t *testing.T) {
	fake := &fakeGCRuntime{}
	h := NewBlockStoreGCHandler(fake)

	req := newGCRequest(http.MethodPost, "/api/v1/shares/myshare/blockstore/gc", "myshare", bytes.NewReader([]byte("{not-json")))
	w := httptest.NewRecorder()

	h.RunGC(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("RunGC: expected 400 on malformed body, got %d (body=%q)", w.Code, w.Body.String())
	}
	if len(fake.startCalls) != 0 {
		t.Fatalf("RunGC: runtime must not be invoked on bad input; got %d calls", len(fake.startCalls))
	}
}

// TestBlockStoreHandler_RunGC_ShareNotFound returns 404 when StartBlockGC
// rejects an unknown share.
func TestBlockStoreHandler_RunGC_ShareNotFound(t *testing.T) {
	fake := &fakeGCRuntime{startErr: fmt.Errorf("%w: %q", shares.ErrShareNotFound, "ghost")}
	h := NewBlockStoreGCHandler(fake)

	body, _ := json.Marshal(BlockStoreGCRequest{})
	req := newGCRequest(http.MethodPost, "/api/v1/shares/ghost/blockstore/gc", "ghost", bytes.NewReader(body))
	w := httptest.NewRecorder()

	h.RunGC(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("RunGC: expected 404, got %d (body=%q)", w.Code, w.Body.String())
	}
}

// TestBlockStoreHandler_RunGC_EmptyShareName returns 400 when {name} is empty.
func TestBlockStoreHandler_RunGC_EmptyShareName(t *testing.T) {
	fake := &fakeGCRuntime{}
	h := NewBlockStoreGCHandler(fake)

	req := newGCRequest(http.MethodPost, "/api/v1/shares//blockstore/gc", "", nil)
	w := httptest.NewRecorder()

	h.RunGC(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("RunGC: expected 400 on empty share name, got %d", w.Code)
	}
}

// TestBlockStoreHandler_RunGC_NilRuntime fails closed when wired with a nil
// runtime.
func TestBlockStoreHandler_RunGC_NilRuntime(t *testing.T) {
	h := NewBlockStoreGCHandler(nil)

	req := newGCRequest(http.MethodPost, "/api/v1/shares/myshare/blockstore/gc", "myshare", nil)
	w := httptest.NewRecorder()

	h.RunGC(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("RunGC: expected 500 on nil runtime, got %d", w.Code)
	}
}

// TestBlockStoreHandler_RunGC_NormalizesShareName guards that the bare chi URL
// param is normalized to the registry key before reaching the runtime.
func TestBlockStoreHandler_RunGC_NormalizesShareName(t *testing.T) {
	for _, urlParam := range []string{"myshare", "/myshare"} {
		fake := &fakeGCRuntime{}
		h := NewBlockStoreGCHandler(fake)

		req := newGCRequest(http.MethodPost, "/api/v1/shares/myshare/blockstore/gc", urlParam, nil)
		w := httptest.NewRecorder()
		h.RunGC(w, req)

		if w.Code != http.StatusAccepted {
			t.Fatalf("RunGC(%q): expected 202, got %d", urlParam, w.Code)
		}
		if len(fake.startCalls) != 1 || fake.startCalls[0].share != "/myshare" {
			t.Fatalf("RunGC(%q): expected runtime called with /myshare, got %+v", urlParam, fake.startCalls)
		}
	}
}

// TestBlockStoreHandler_GCJobStatus_Success returns the job for a known id.
func TestBlockStoreHandler_GCJobStatus_Success(t *testing.T) {
	fake := &fakeGCRuntime{
		statusOK: true,
		statusJob: &runtime.GCJob{
			ID: "gc-7", State: runtime.GCStateDone, Share: "/myshare",
			ObjectsSwept: 3, BytesFreed: 2048,
			Stats: &engine.GCStats{RunID: "r-7", ObjectsSwept: 3, BytesFreed: 2048},
		},
	}
	h := NewBlockStoreGCHandler(fake)

	req := newGCJobRequest(http.MethodGet, "/api/v1/shares/myshare/blockstore/gc/gc-7", "myshare", "gc-7", nil)
	w := httptest.NewRecorder()

	h.GCJobStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GCJobStatus: expected 200, got %d (body=%q)", w.Code, w.Body.String())
	}
	var got GCJobStatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("GCJobStatus: decode: %v", err)
	}
	if got.ID != "gc-7" || got.State != runtime.GCStateDone || got.ObjectsSwept != 3 || got.Stats == nil {
		t.Fatalf("GCJobStatus: unexpected body: %+v", got)
	}
}

// TestBlockStoreHandler_GCJobStatus_NotFound returns 404 for an unknown id.
func TestBlockStoreHandler_GCJobStatus_NotFound(t *testing.T) {
	fake := &fakeGCRuntime{statusOK: false}
	h := NewBlockStoreGCHandler(fake)

	req := newGCJobRequest(http.MethodGet, "/api/v1/shares/myshare/blockstore/gc/nope", "myshare", "nope", nil)
	w := httptest.NewRecorder()

	h.GCJobStatus(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("GCJobStatus: expected 404, got %d", w.Code)
	}
}

// TestBlockStoreHandler_GCStatus_Success reads a valid last-run.json from the
// share's gc-state directory and round-trips the parsed GCRunSummary as JSON.
func TestBlockStoreHandler_GCStatus_Success(t *testing.T) {
	root := t.TempDir()
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

// TestBlockStoreHandler_GCStatus_NoRunYet returns 404 when last-run.json does
// not exist (the share has never run GC).
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

// TestBlockStoreHandler_GCStatus_EmptyRoot returns 404 when the share's local
// store has no persistent root (in-memory backend).
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

// TestBlockStoreHandler_GCStatus_ShareNotFound returns 404 when the share is
// unknown (GCStateDirForShare wraps shares.ErrShareNotFound).
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

// TestBlockStoreHandler_GCStatus_MalformedFile returns 500 when last-run.json
// exists but cannot be parsed.
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

// TestBlockStoreHandler_GCStatus_NilRuntime fails closed when wired with a nil
// runtime.
func TestBlockStoreHandler_GCStatus_NilRuntime(t *testing.T) {
	h := NewBlockStoreGCHandler(nil)

	req := newGCRequest(http.MethodGet, "/api/v1/shares/myshare/blockstore/gc-status", "myshare", nil)
	w := httptest.NewRecorder()

	h.GCStatus(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("GCStatus: expected 500 on nil runtime, got %d", w.Code)
	}
}
