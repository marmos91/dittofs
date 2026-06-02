package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
)

// fakeAuditRuntime is a recording stand-in for handlers.BlockAuditRuntime.
// Tests assert on captured shares and feed canned results.
type fakeAuditRuntime struct {
	res   *engine.AuditRefcountsResult
	err   error
	calls []string
}

func (f *fakeAuditRuntime) AuditRefcounts(_ context.Context, shareName string) (*engine.AuditRefcountsResult, error) {
	f.calls = append(f.calls, shareName)
	if f.err != nil {
		return nil, f.err
	}
	return f.res, nil
}

func newAuditRequest(method, path, share string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", share)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

// TestBlockStoreAuditHandler_RunAudit_Success asserts a POST invokes
// AuditRefcounts with the share name and round-trips the result as JSON.
func TestBlockStoreAuditHandler_RunAudit_Success(t *testing.T) {
	fake := &fakeAuditRuntime{
		res: &engine.AuditRefcountsResult{
			Share:         "myshare",
			TotalFiles:    3,
			TotalRefs:     10,
			TotalRefCount: 10,
			Delta:         0,
			DurationMS:    42,
		},
	}
	h := NewBlockStoreAuditHandler(fake)

	req := newAuditRequest(http.MethodPost, "/api/v1/shares/myshare/audit/refcounts", "myshare")
	w := httptest.NewRecorder()

	h.RunAudit(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("RunAudit: expected 200, got %d (body=%q)", w.Code, w.Body.String())
	}
	if len(fake.calls) != 1 || fake.calls[0] != "myshare" {
		t.Fatalf("RunAudit: expected single call for myshare, got %+v", fake.calls)
	}

	var resp BlockStoreAuditResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Result == nil {
		t.Fatal("RunAudit: nil Result in response")
	}
	if resp.Result.TotalFiles != 3 || resp.Result.TotalRefs != 10 || resp.Result.TotalRefCount != 10 || resp.Result.Delta != 0 {
		t.Fatalf("RunAudit: unexpected result: %+v", resp.Result)
	}
}

// TestBlockStoreAuditHandler_RunAudit_Drift carries Delta != 0 through
// the response so operators can branch scripts on `delta != 0`.
func TestBlockStoreAuditHandler_RunAudit_Drift(t *testing.T) {
	fake := &fakeAuditRuntime{
		res: &engine.AuditRefcountsResult{
			Share:         "myshare",
			TotalFiles:    3,
			TotalRefs:     10,
			TotalRefCount: 15,
			Delta:         -5,
		},
	}
	h := NewBlockStoreAuditHandler(fake)

	req := newAuditRequest(http.MethodPost, "/api/v1/shares/myshare/audit/refcounts", "myshare")
	w := httptest.NewRecorder()
	h.RunAudit(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("RunAudit (drift): expected 200, got %d", w.Code)
	}
	var resp BlockStoreAuditResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Result.Delta != -5 {
		t.Fatalf("Delta = %d, want -5", resp.Result.Delta)
	}
}

// TestBlockStoreAuditHandler_RunAudit_ShareNotFound returns 404 when the
// share is unknown.
func TestBlockStoreAuditHandler_RunAudit_ShareNotFound(t *testing.T) {
	fake := &fakeAuditRuntime{
		err: fmt.Errorf("%w: %q", shares.ErrShareNotFound, "ghost"),
	}
	h := NewBlockStoreAuditHandler(fake)

	req := newAuditRequest(http.MethodPost, "/api/v1/shares/ghost/audit/refcounts", "ghost")
	w := httptest.NewRecorder()
	h.RunAudit(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d (body=%q)", w.Code, w.Body.String())
	}
}

// TestBlockStoreAuditHandler_RunAudit_EmptyShareName returns 400 when
// the URL parameter is empty.
func TestBlockStoreAuditHandler_RunAudit_EmptyShareName(t *testing.T) {
	fake := &fakeAuditRuntime{}
	h := NewBlockStoreAuditHandler(fake)

	req := newAuditRequest(http.MethodPost, "/api/v1/shares//audit/refcounts", "")
	w := httptest.NewRecorder()
	h.RunAudit(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	if len(fake.calls) != 0 {
		t.Fatal("runtime must not be invoked on empty share")
	}
}

// TestBlockStoreAuditHandler_RunAudit_NilRuntime fails closed when the
// handler is wired with a nil runtime.
func TestBlockStoreAuditHandler_RunAudit_NilRuntime(t *testing.T) {
	h := NewBlockStoreAuditHandler(nil)

	req := newAuditRequest(http.MethodPost, "/api/v1/shares/myshare/audit/refcounts", "myshare")
	w := httptest.NewRecorder()
	h.RunAudit(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on nil runtime, got %d", w.Code)
	}
}

// TestBlockStoreAuditHandler_RunAudit_RuntimeError surfaces non-share
// errors as 500 with a sanitized body (no path leaks).
func TestBlockStoreAuditHandler_RunAudit_RuntimeError(t *testing.T) {
	fake := &fakeAuditRuntime{err: fmt.Errorf("internal: %s", "/secret/path")}
	h := NewBlockStoreAuditHandler(fake)

	req := newAuditRequest(http.MethodPost, "/api/v1/shares/myshare/audit/refcounts", "myshare")
	w := httptest.NewRecorder()
	h.RunAudit(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
	body := w.Body.String()
	if containsPath(body, "/secret/path") {
		t.Errorf("response body leaks underlying error path: %q", body)
	}
}

// containsPath checks if the response body literally contains the
// secret path; used to verify error sanitization.
func containsPath(body, path string) bool {
	return len(body) > 0 && len(path) > 0 && (indexOf(body, path) >= 0)
}

// indexOf is a stripped strings.Index without importing the strings
// package into the test file.
func indexOf(haystack, needle string) int {
	if len(needle) == 0 {
		return 0
	}
	if len(haystack) < len(needle) {
		return -1
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
