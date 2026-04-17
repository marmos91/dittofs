package backup

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
)

// pinHandler captures the PATCH body so tests can assert Pinned=true/false.
type pinHandler struct {
	receivedPinned *bool
	returnRec      apiclient.BackupRecord
}

func (h *pinHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPatch {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Pinned *bool `json:"pinned"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	h.receivedPinned = body.Pinned
	if body.Pinned != nil {
		h.returnRec.Pinned = *body.Pinned
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(h.returnRec)
}

func TestBackupPin_Succeeds(t *testing.T) {
	f := newRunTestFixture(t)
	cmdutil.Flags.Output = "json"

	h := &pinHandler{returnRec: apiclient.BackupRecord{ID: "01HARECORD0000000000000000"}}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	withClient(t, srv.URL)

	if err := runPin(Cmd, []string{"fast-meta", "01HARECORD0000000000000000"}); err != nil {
		t.Fatalf("runPin err: %v", err)
	}
	if h.receivedPinned == nil || !*h.receivedPinned {
		t.Fatalf("expected Pinned=true on PATCH body, got %+v", h.receivedPinned)
	}
	// JSON mode: the resource is serialised on stdout.
	if !strings.Contains(f.stdout.String(), "01HARECORD") {
		t.Errorf("expected record JSON in stdout; got %q", f.stdout.String())
	}
}

func TestBackupUnpin_Succeeds(t *testing.T) {
	newRunTestFixture(t)
	cmdutil.Flags.Output = "json"

	h := &pinHandler{returnRec: apiclient.BackupRecord{ID: "01HARECORD0000000000000000", Pinned: true}}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	withClient(t, srv.URL)

	if err := runUnpin(Cmd, []string{"fast-meta", "01HARECORD0000000000000000"}); err != nil {
		t.Fatalf("runUnpin err: %v", err)
	}
	if h.receivedPinned == nil || *h.receivedPinned {
		t.Fatalf("expected Pinned=false on PATCH body, got %+v", h.receivedPinned)
	}
}
