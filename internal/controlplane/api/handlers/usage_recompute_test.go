package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
)

// fakeUsageRecomputeRuntime is a recording stand-in for
// handlers.UsageRecomputeRuntime.
type fakeUsageRecomputeRuntime struct {
	res     *runtime.UsageRecomputeResult
	err     error
	calls   []string
	dryRuns []bool
}

func (f *fakeUsageRecomputeRuntime) RecomputeShareUsage(_ context.Context, shareName string, dryRun bool) (*runtime.UsageRecomputeResult, error) {
	f.calls = append(f.calls, shareName)
	f.dryRuns = append(f.dryRuns, dryRun)
	if f.err != nil {
		return nil, f.err
	}
	return f.res, nil
}

func newRecomputeRequest(share string) *http.Request {
	return newRecomputeRequestRaw(share, "/api/v1/shares/"+share+"/usage/recompute")
}

func newRecomputeRequestRaw(share, target string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, target, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", share)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

func TestUsageRecomputeHandler_Success(t *testing.T) {
	fake := &fakeUsageRecomputeRuntime{
		res: &runtime.UsageRecomputeResult{
			ShareName:   "/myshare",
			BeforeBytes: 1256,
			AfterBytes:  256,
			DurationMS:  7,
		},
	}
	h := NewUsageRecomputeHandler(fake)

	w := httptest.NewRecorder()
	h.Recompute(w, newRecomputeRequest("myshare"))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var got UsageRecomputeResponse
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Result == nil || got.Result.BeforeBytes != 1256 || got.Result.AfterBytes != 256 {
		t.Fatalf("result = %+v, want the runtime's before/after round-tripped", got.Result)
	}
	if len(fake.calls) != 1 || fake.calls[0] != "/myshare" {
		t.Fatalf("runtime calls = %v, want one call for /myshare", fake.calls)
	}
}

func TestUsageRecomputeHandler_UnknownShare(t *testing.T) {
	fake := &fakeUsageRecomputeRuntime{err: fmt.Errorf("resolve: %w", shares.ErrShareNotFound)}
	h := NewUsageRecomputeHandler(fake)

	w := httptest.NewRecorder()
	h.Recompute(w, newRecomputeRequest("ghost"))

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

// TestUsageRecomputeHandler_ErrorNotEchoed pins that an unexpected failure is
// reported without the underlying error text, which can carry DSNs and paths.
func TestUsageRecomputeHandler_ErrorNotEchoed(t *testing.T) {
	fake := &fakeUsageRecomputeRuntime{err: fmt.Errorf("dial postgres://user:hunter2@db:5432: refused")}
	h := NewUsageRecomputeHandler(fake)

	w := httptest.NewRecorder()
	h.Recompute(w, newRecomputeRequest("myshare"))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}
	if body := w.Body.String(); strings.Contains(body, "hunter2") {
		t.Fatalf("response body leaked the underlying error: %s", body)
	}
}

// TestUsageRecomputeHandler_DryRun pins that the query parameter reaches the
// runtime. Defaulting it to false on a parse slip would silently run the
// repair an operator asked only to preview.
func TestUsageRecomputeHandler_DryRun(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{"?dry_run=true", true},
		{"?dry_run=1", true},
		{"?dry_run=false", false},
		{"", false},
	} {
		fake := &fakeUsageRecomputeRuntime{res: &runtime.UsageRecomputeResult{ShareName: "/myshare"}}
		h := NewUsageRecomputeHandler(fake)

		w := httptest.NewRecorder()
		h.Recompute(w, newRecomputeRequestRaw("myshare", "/api/v1/shares/myshare/usage/recompute"+tc.raw))

		if w.Code != http.StatusOK {
			t.Fatalf("%q: status = %d, want %d", tc.raw, w.Code, http.StatusOK)
		}
		if len(fake.dryRuns) != 1 || fake.dryRuns[0] != tc.want {
			t.Fatalf("%q: runtime saw dryRun = %v, want [%v]", tc.raw, fake.dryRuns, tc.want)
		}
	}
}

// TestUsageRecomputeHandler_DryRunNotABool pins that an unparseable value is
// refused rather than falling back to the repair.
func TestUsageRecomputeHandler_DryRunNotABool(t *testing.T) {
	fake := &fakeUsageRecomputeRuntime{res: &runtime.UsageRecomputeResult{ShareName: "/myshare"}}
	h := NewUsageRecomputeHandler(fake)

	w := httptest.NewRecorder()
	h.Recompute(w, newRecomputeRequestRaw("myshare", "/api/v1/shares/myshare/usage/recompute?dry_run=maybe"))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("runtime was called %v, want no call at all", fake.calls)
	}
}
