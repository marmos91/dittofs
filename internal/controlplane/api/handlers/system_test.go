package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// fakeDrainRuntime is a minimal drainRuntime test double. drainFn overrides
// DrainAllUploads; uploadProgressFn overrides UploadProgress. The zero value
// drains instantly with no error and reports no progress.
type fakeDrainRuntime struct {
	drainFn          func(ctx context.Context) error
	uploadProgressFn func() int64
}

func (f *fakeDrainRuntime) DrainAllUploads(ctx context.Context) error {
	if f.drainFn != nil {
		return f.drainFn(ctx)
	}
	return nil
}

func (f *fakeDrainRuntime) UploadProgress() int64 {
	if f.uploadProgressFn != nil {
		return f.uploadProgressFn()
	}
	return 0
}

// newSystemRouterWithGlobalTimeout mirrors the production router's short global
// request timeout (middleware.Timeout) applied to the drain route, so a test
// can assert the drain handler is not cancelled by it (issue #1432).
func newSystemRouterWithGlobalTimeout(h *SystemHandler, d time.Duration) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Timeout(d))
	r.Route("/api/v1/system", func(r chi.Router) {
		r.Post("/drain-uploads", h.DrainUploads)
	})
	return r
}

// TestSystemHandler_DrainUploads_DecoupledFromGlobalTimeout asserts that the
// router's short global request timeout does not abort a drain whose own budget
// is larger — a real multi-GiB flush must be allowed to run past the global
// handler deadline AND the client must still receive the real 200 success
// response, not the middleware's 504 (issue #1432).
func TestSystemHandler_DrainUploads_DecoupledFromGlobalTimeout(t *testing.T) {
	gotCtxErr := make(chan error, 1)
	fake := &fakeDrainRuntime{
		drainFn: func(ctx context.Context) error {
			// Simulate a flush that outlasts the global 20ms timeout but
			// finishes well inside the stall budget.
			select {
			case <-ctx.Done():
				gotCtxErr <- ctx.Err()
				return ctx.Err()
			case <-time.After(80 * time.Millisecond):
				gotCtxErr <- nil
				return nil
			}
		},
	}
	h := &SystemHandler{runtime: fake, drainStallTimeout: time.Minute}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/system/drain-uploads", nil)
	rr := httptest.NewRecorder()
	newSystemRouterWithGlobalTimeout(h, 20*time.Millisecond).ServeHTTP(rr, req)

	select {
	case err := <-gotCtxErr:
		if err != nil {
			t.Fatalf("drain was cancelled by the global request timeout: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("drainFn never returned")
	}

	// The decoupling is only real if the client gets the genuine success
	// response. chi middleware.Timeout fires its deferred 504 WriteHeader after
	// the handler returns (the request context's deadline elapsed during the
	// 80ms drain); the handler must have already written 200 + body so that
	// stale 504 is a no-op.
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (middleware 504 must not clobber the success response): body=%s",
			rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode body: %v (body=%s)", err, rr.Body.String())
	}
	if got["status"] != "drained" {
		t.Fatalf("body = %+v, want status=drained", got)
	}
}

// TestSystemHandler_DrainUploads_ClientDisconnectStopsDrain asserts that a
// genuine client disconnect (request context Canceled, not the deadline fired
// by middleware.Timeout) still aborts the drain — decoupling from the global
// timeout must not make the drain ignore an aborted request.
func TestSystemHandler_DrainUploads_ClientDisconnectStopsDrain(t *testing.T) {
	started := make(chan struct{})
	gotCtxErr := make(chan error, 1)
	fake := &fakeDrainRuntime{
		drainFn: func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			gotCtxErr <- ctx.Err()
			return ctx.Err()
		},
	}
	h := &SystemHandler{runtime: fake, drainStallTimeout: time.Minute}

	clientCtx, clientCancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/system/drain-uploads", nil).
		WithContext(clientCtx)
	rr := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		h.DrainUploads(rr, req)
		close(done)
	}()

	<-started
	clientCancel() // simulate the client hanging up

	select {
	case err := <-gotCtxErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("drain ctx err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("client disconnect did not stop the drain")
	}
	<-done
}

// TestSystemHandler_DrainUploads_StallReturns504 asserts that a drain that
// makes no upload progress for the stall window is aborted and reported as 504.
// This is the core of the inactivity model: a wedged remote must not hang the
// request forever.
func TestSystemHandler_DrainUploads_StallReturns504(t *testing.T) {
	fake := &fakeDrainRuntime{
		drainFn: func(ctx context.Context) error {
			// Never makes progress; unblocks only when the watchdog cancels.
			<-ctx.Done()
			return ctx.Err()
		},
		// uploadProgressFn nil → constant 0 → watchdog sees a flat counter.
	}
	h := &SystemHandler{runtime: fake, drainStallTimeout: 20 * time.Millisecond}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/system/drain-uploads", nil)
	rr := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		h.DrainUploads(rr, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("stalled drain did not return")
	}
	if rr.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504 for a stalled drain: body=%s", rr.Code, rr.Body.String())
	}
}

// TestSystemHandler_DrainUploads_ProgressResetsWatchdog proves there is no
// total wall-clock cap: a drain that runs far longer than the stall window
// still succeeds as long as it keeps making progress. The total runtime (~120ms
// in 30ms steps) deliberately exceeds the 50ms stall budget — a fixed total
// timeout would wrongly abort it; the inactivity watchdog must not.
func TestSystemHandler_DrainUploads_ProgressResetsWatchdog(t *testing.T) {
	var progress atomic.Int64
	fake := &fakeDrainRuntime{
		uploadProgressFn: progress.Load,
		drainFn: func(ctx context.Context) error {
			for i := 0; i < 4; i++ {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(30 * time.Millisecond):
					progress.Add(1) // a chunk completed → resets the watchdog
				}
			}
			return nil
		},
	}
	h := &SystemHandler{runtime: fake, drainStallTimeout: 50 * time.Millisecond}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/system/drain-uploads", nil)
	rr := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		h.DrainUploads(rr, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("drain did not return")
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (progress must reset the stall watchdog): body=%s",
			rr.Code, rr.Body.String())
	}
}

// TestSystemHandler_DrainUploads_NilRuntime returns 500 rather than panicking
// when the handler was constructed without a runtime.
func TestSystemHandler_DrainUploads_NilRuntime(t *testing.T) {
	h := NewSystemHandler(nil, 0)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/system/drain-uploads", nil)
	rr := httptest.NewRecorder()
	h.DrainUploads(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
}
