package backup

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/apiclient"
)

// newListServer returns a test server that answers GET /backups and verifies
// the optional ?repo= query parameter when expectedRepo is non-empty.
func newListServer(t *testing.T, records []apiclient.BackupRecord, expectedRepo string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/backups") {
			http.NotFound(w, r)
			return
		}
		if expectedRepo != "" && r.URL.Query().Get("repo") != expectedRepo {
			http.Error(w, "unexpected repo filter", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(records)
	}))
	t.Cleanup(s.Close)
	return s
}

func TestBackupList_Table_D26Columns(t *testing.T) {
	f := newRunTestFixture(t)

	records := []apiclient.BackupRecord{
		{
			ID:        "01HABCDEFGHJKMNPQRSTUVWXYZ",
			RepoID:    "daily-s3",
			Status:    "succeeded",
			SizeBytes: 1024 * 1024,
			CreatedAt: time.Now().Add(-3 * time.Hour),
		},
		{
			ID:        "01HWXYZ0000000000000000000",
			RepoID:    "daily-s3",
			Status:    "succeeded",
			SizeBytes: 2048,
			Pinned:    true,
			CreatedAt: time.Now().Add(-30 * time.Minute),
		},
	}
	srv := newListServer(t, records, "daily-s3")
	withClient(t, srv.URL)

	prev := listRepo
	listRepo = "daily-s3"
	t.Cleanup(func() { listRepo = prev })

	if err := runList(Cmd, []string{"fast-meta"}); err != nil {
		t.Fatalf("runList err: %v", err)
	}
	out := f.stdout.String()
	for _, header := range []string{"ID", "CREATED", "SIZE", "STATUS", "REPO", "PINNED"} {
		if !strings.Contains(out, header) {
			t.Errorf("missing header %q in output: %q", header, out)
		}
	}
	if !strings.Contains(out, "01HABCDE\u2026") {
		t.Errorf("expected short ULID in output; got %q", out)
	}
	if !strings.Contains(out, "yes") {
		t.Errorf("expected 'yes' for pinned row; got %q", out)
	}
}

func TestBackupList_Empty_ShowsHint(t *testing.T) {
	f := newRunTestFixture(t)

	srv := newListServer(t, []apiclient.BackupRecord{}, "")
	withClient(t, srv.URL)

	prev := listRepo
	listRepo = ""
	t.Cleanup(func() { listRepo = prev })

	if err := runList(Cmd, []string{"fast-meta"}); err != nil {
		t.Fatalf("runList err: %v", err)
	}
	if !strings.Contains(f.stdout.String(), "No backups yet") {
		t.Errorf("expected empty hint; got %q", f.stdout.String())
	}
	if !strings.Contains(f.stdout.String(), "dfsctl store metadata fast-meta backup") {
		t.Errorf("expected next-step hint; got %q", f.stdout.String())
	}
}

func TestBackupList_RepoFlagIncluded(t *testing.T) {
	f := newRunTestFixture(t)

	srv := newListServer(t, []apiclient.BackupRecord{}, "daily-s3")
	withClient(t, srv.URL)

	prev := listRepo
	listRepo = "daily-s3"
	t.Cleanup(func() { listRepo = prev })

	if err := runList(Cmd, []string{"fast-meta"}); err != nil {
		t.Fatalf("runList err: %v", err)
	}
	// Empty-mode hint must include --repo when listRepo was set.
	if !strings.Contains(f.stdout.String(), "--repo daily-s3") {
		t.Errorf("expected --repo daily-s3 in empty hint; got %q", f.stdout.String())
	}
}
