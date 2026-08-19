package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
)

// fakeManifestCheckRuntime is a recording stand-in for ManifestCheckRuntime.
type fakeManifestCheckRuntime struct {
	res   *engine.ManifestCheckResult
	err   error
	calls []string
}

func (f *fakeManifestCheckRuntime) CheckManifests(_ context.Context, shareName string) (*engine.ManifestCheckResult, error) {
	f.calls = append(f.calls, shareName)
	if f.err != nil {
		return nil, f.err
	}
	return f.res, nil
}

func runManifestCheck(rt ManifestCheckRuntime, share string) *httptest.ResponseRecorder {
	req := newAuditRequest(http.MethodPost, "/api/v1/shares/"+share+"/audit/manifest", share)
	w := httptest.NewRecorder()
	NewBlockStoreManifestCheckHandler(rt).RunManifestCheck(w, req)
	return w
}

// TestManifestCheckHandler_Success asserts the handler normalizes the share
// name, invokes the runtime once, and round-trips the findings as JSON.
func TestManifestCheckHandler_Success(t *testing.T) {
	fake := &fakeManifestCheckRuntime{res: &engine.ManifestCheckResult{
		Share:                  "/myshare",
		FilesScanned:           3,
		DamagedPayloads:        1,
		UncoveredRanges:        2,
		UncoveredBytes:         8192,
		ClaimedUncoveredRanges: 1,
		ClaimedUncoveredBytes:  4096,
		Findings: []engine.PayloadFinding{{
			Path:      "/docs/report.pdf",
			PayloadID: "payload-1",
			Size:      1048576,
			Damaged:   true,
			Uncovered: []engine.ByteRange{{Start: 0, End: 4096, Claimed: true}},
		}},
	}}

	w := runManifestCheck(fake, "myshare")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%q)", w.Code, w.Body.String())
	}
	if len(fake.calls) != 1 || fake.calls[0] != "/myshare" {
		t.Fatalf("expected a single call for /myshare, got %+v", fake.calls)
	}

	var resp BlockStoreManifestCheckResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Result == nil || resp.Result.DamagedPayloads != 1 || resp.Result.ClaimedUncoveredBytes != 4096 {
		t.Fatalf("unexpected result: %+v", resp.Result)
	}
	if len(resp.Result.Findings) != 1 || !resp.Result.Findings[0].Damaged {
		t.Fatalf("per-payload detail lost in transit: %+v", resp.Result.Findings)
	}
	if got := resp.Result.Findings[0].Uncovered; len(got) != 1 || !got[0].Claimed {
		t.Fatalf("claimed flag lost in transit: %+v", got)
	}
}

// TestManifestCheckHandler_ShareNotFound maps the share sentinel to 404.
func TestManifestCheckHandler_ShareNotFound(t *testing.T) {
	fake := &fakeManifestCheckRuntime{err: fmt.Errorf("%w: %q", shares.ErrShareNotFound, "ghost")}
	if w := runManifestCheck(fake, "ghost"); w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d (body=%q)", w.Code, w.Body.String())
	}
}

// TestManifestCheckHandler_EmptyShareName rejects an empty URL parameter
// without reaching the runtime.
func TestManifestCheckHandler_EmptyShareName(t *testing.T) {
	fake := &fakeManifestCheckRuntime{}
	if w := runManifestCheck(fake, ""); w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	if len(fake.calls) != 0 {
		t.Fatal("runtime must not be invoked on an empty share name")
	}
}

// TestManifestCheckHandler_NilRuntime fails closed rather than panicking.
func TestManifestCheckHandler_NilRuntime(t *testing.T) {
	if w := runManifestCheck(nil, "myshare"); w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on nil runtime, got %d", w.Code)
	}
}

// TestManifestCheckHandler_RuntimeError surfaces an unexpected error as 500
// with the underlying message stripped — it can carry filesystem paths.
func TestManifestCheckHandler_RuntimeError(t *testing.T) {
	fake := &fakeManifestCheckRuntime{err: fmt.Errorf("walk failed: %s", "/secret/path")}
	w := runManifestCheck(fake, "myshare")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "/secret/path") {
		t.Errorf("response body leaks the underlying error path: %q", w.Body.String())
	}
}
