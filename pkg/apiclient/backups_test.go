package apiclient

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newStubServer builds an httptest.Server that records request method+path
// and echoes back a canned response + status+contenttype.
type stubCall struct {
	Method string
	Path   string
	Body   []byte
}

type stubServer struct {
	*httptest.Server
	calls []stubCall

	// Response controls
	status      int
	body        []byte
	contentType string
}

func (s *stubServer) reset() {
	s.calls = nil
	s.status = http.StatusOK
	s.body = nil
	s.contentType = "application/json"
}

func newStubServer(t *testing.T) *stubServer {
	t.Helper()
	s := &stubServer{status: http.StatusOK, contentType: "application/json"}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		s.calls = append(s.calls, stubCall{Method: r.Method, Path: r.URL.RequestURI(), Body: b})
		if s.contentType != "" {
			w.Header().Set("Content-Type", s.contentType)
		}
		w.WriteHeader(s.status)
		if s.body != nil {
			_, _ = w.Write(s.body)
		}
	}))
	return s
}

// newTestClient returns a Client pointed at the stub server.
func newTestClient(s *stubServer) *Client {
	return New(s.URL)
}

func TestClient_TriggerBackup_PostsCorrectPath_Singular(t *testing.T) {
	s := newStubServer(t)
	defer s.Close()

	s.reset()
	s.status = http.StatusAccepted
	s.body = []byte(`{"record":{"id":"r1"},"job":{"id":"j1"}}`)

	c := newTestClient(s)
	resp, err := c.TriggerBackup("fast-meta", &TriggerBackupRequest{})
	if err != nil {
		t.Fatalf("TriggerBackup: %v", err)
	}
	if len(s.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(s.calls))
	}
	if s.calls[0].Method != http.MethodPost {
		t.Errorf("method = %q, want POST", s.calls[0].Method)
	}
	if s.calls[0].Path != "/api/v1/store/metadata/fast-meta/backups" {
		t.Errorf("path = %q, want singular /store/metadata/fast-meta/backups", s.calls[0].Path)
	}
	if resp.Record == nil || resp.Record.ID != "r1" {
		t.Errorf("record ID = %+v", resp.Record)
	}
	if resp.Job == nil || resp.Job.ID != "j1" {
		t.Errorf("job ID = %+v", resp.Job)
	}
}

func TestClient_ListBackupRecords_BuildsRepoQuery(t *testing.T) {
	s := newStubServer(t)
	defer s.Close()
	s.reset()
	s.body = []byte(`[]`)

	c := newTestClient(s)
	if _, err := c.ListBackupRecords("fast-meta", "primary"); err != nil {
		t.Fatalf("ListBackupRecords: %v", err)
	}
	if s.calls[0].Path != "/api/v1/store/metadata/fast-meta/backups?repo=primary" {
		t.Errorf("path = %q, want repo query", s.calls[0].Path)
	}
}

func TestClient_StartRestore_Returns202AndJob(t *testing.T) {
	s := newStubServer(t)
	defer s.Close()
	s.reset()
	s.status = http.StatusAccepted
	s.body = []byte(`{"id":"j1","kind":"restore","status":"running"}`)

	c := newTestClient(s)
	job, err := c.StartRestore("fast-meta", &RestoreRequest{})
	if err != nil {
		t.Fatalf("StartRestore: %v", err)
	}
	if job.ID != "j1" || job.Kind != "restore" {
		t.Errorf("job = %+v", job)
	}
	if s.calls[0].Path != "/api/v1/store/metadata/fast-meta/restore" {
		t.Errorf("path = %q", s.calls[0].Path)
	}
}

func TestClient_RestoreDryRun_Returns200AndResult(t *testing.T) {
	s := newStubServer(t)
	defer s.Close()
	s.reset()
	s.body = []byte(`{"record":{"id":"r1"},"manifest_valid":true,"enabled_shares":["/a"]}`)

	c := newTestClient(s)
	res, err := c.RestoreDryRun("fast-meta", &RestoreRequest{})
	if err != nil {
		t.Fatalf("RestoreDryRun: %v", err)
	}
	if !res.ManifestValid || len(res.EnabledShares) != 1 {
		t.Errorf("res = %+v", res)
	}
	if s.calls[0].Path != "/api/v1/store/metadata/fast-meta/restore/dry-run" {
		t.Errorf("path = %q", s.calls[0].Path)
	}
}

func TestClient_BackupAlreadyRunning_SurfacesRunningJobID(t *testing.T) {
	s := newStubServer(t)
	defer s.Close()
	s.reset()
	s.status = http.StatusConflict
	s.contentType = "application/problem+json"
	s.body = []byte(`{"title":"Conflict","status":409,"detail":"backup already running","running_job_id":"01HABC"}`)

	c := newTestClient(s)
	_, err := c.TriggerBackup("fast-meta", &TriggerBackupRequest{})
	if err == nil {
		t.Fatalf("expected error")
	}
	var typed *BackupAlreadyRunningError
	if !errors.As(err, &typed) {
		t.Fatalf("err should be *BackupAlreadyRunningError: %T %v", err, err)
	}
	if typed.RunningJobID != "01HABC" {
		t.Errorf("RunningJobID = %q, want 01HABC", typed.RunningJobID)
	}
}

func TestClient_RestorePreconditionFailed_SurfacesEnabledShares(t *testing.T) {
	s := newStubServer(t)
	defer s.Close()
	s.reset()
	s.status = http.StatusConflict
	s.contentType = "application/problem+json"
	s.body = []byte(`{"title":"Restore precondition failed","status":409,"detail":"1 share(s) still enabled","enabled_shares":["/a"]}`)

	c := newTestClient(s)
	_, err := c.StartRestore("fast-meta", &RestoreRequest{})
	if err == nil {
		t.Fatalf("expected error")
	}
	var typed *RestorePreconditionError
	if !errors.As(err, &typed) {
		t.Fatalf("err should be *RestorePreconditionError: %T %v", err, err)
	}
	if len(typed.EnabledShares) != 1 || typed.EnabledShares[0] != "/a" {
		t.Errorf("EnabledShares = %v, want [/a]", typed.EnabledShares)
	}
}

func TestClient_GetSetPinnedRecord(t *testing.T) {
	s := newStubServer(t)
	defer s.Close()
	s.reset()
	s.body = []byte(`{"id":"r1","pinned":true}`)

	c := newTestClient(s)
	rec, err := c.SetBackupRecordPinned("fast-meta", "r1", true)
	if err != nil {
		t.Fatalf("SetBackupRecordPinned: %v", err)
	}
	if !rec.Pinned {
		t.Errorf("pinned not echoed")
	}
	if s.calls[0].Method != http.MethodPatch {
		t.Errorf("method = %q, want PATCH", s.calls[0].Method)
	}
	if s.calls[0].Path != "/api/v1/store/metadata/fast-meta/backups/r1" {
		t.Errorf("path = %q", s.calls[0].Path)
	}
	var sent PatchBackupRecordRequest
	if err := json.Unmarshal(s.calls[0].Body, &sent); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if sent.Pinned == nil || !*sent.Pinned {
		t.Errorf("sent body did not carry pinned=true: %+v", sent)
	}
}

func TestClient_ListBackupJobs_BuildsFilterQuery(t *testing.T) {
	s := newStubServer(t)
	defer s.Close()
	s.reset()
	s.body = []byte(`[]`)

	c := newTestClient(s)
	_, err := c.ListBackupJobs("fast-meta", BackupJobFilter{
		RepoName: "primary",
		Status:   "running",
		Kind:     "backup",
		Limit:    10,
	})
	if err != nil {
		t.Fatalf("ListBackupJobs: %v", err)
	}
	p := s.calls[0].Path
	for _, want := range []string{"repo=primary", "status=running", "kind=backup", "limit=10", "/backup-jobs"} {
		if !strings.Contains(p, want) {
			t.Errorf("path %q missing %q", p, want)
		}
	}
}

func TestClient_CancelBackupJob_Terminal_Returns200(t *testing.T) {
	s := newStubServer(t)
	defer s.Close()
	s.reset()
	s.status = http.StatusOK
	s.body = []byte(`{"id":"j1","status":"succeeded"}`)

	c := newTestClient(s)
	job, err := c.CancelBackupJob("fast-meta", "j1")
	if err != nil {
		t.Fatalf("CancelBackupJob: %v", err)
	}
	if job.Status != "succeeded" {
		t.Errorf("status = %q", job.Status)
	}
	if s.calls[0].Path != "/api/v1/store/metadata/fast-meta/backup-jobs/j1/cancel" {
		t.Errorf("path = %q", s.calls[0].Path)
	}
}

func TestClient_RepoCRUD_Paths(t *testing.T) {
	s := newStubServer(t)
	defer s.Close()

	c := newTestClient(s)

	tests := []struct {
		name   string
		fn     func() error
		method string
		path   string
	}{
		{
			name: "Create",
			fn: func() error {
				s.reset()
				s.status = http.StatusCreated
				s.body = []byte(`{"id":"rp1","name":"primary"}`)
				_, err := c.CreateBackupRepo("fast-meta", &BackupRepoRequest{Name: "primary", Kind: "local"})
				return err
			},
			method: http.MethodPost,
			path:   "/api/v1/store/metadata/fast-meta/repos",
		},
		{
			name: "List",
			fn: func() error {
				s.reset()
				s.body = []byte(`[]`)
				_, err := c.ListBackupRepos("fast-meta")
				return err
			},
			method: http.MethodGet,
			path:   "/api/v1/store/metadata/fast-meta/repos",
		},
		{
			name: "Get",
			fn: func() error {
				s.reset()
				s.body = []byte(`{"id":"rp1","name":"primary"}`)
				_, err := c.GetBackupRepo("fast-meta", "primary")
				return err
			},
			method: http.MethodGet,
			path:   "/api/v1/store/metadata/fast-meta/repos/primary",
		},
		{
			name: "Update",
			fn: func() error {
				s.reset()
				s.body = []byte(`{"id":"rp1","name":"primary"}`)
				_, err := c.UpdateBackupRepo("fast-meta", "primary", &BackupRepoRequest{})
				return err
			},
			method: http.MethodPatch,
			path:   "/api/v1/store/metadata/fast-meta/repos/primary",
		},
		{
			name: "Delete",
			fn: func() error {
				s.reset()
				s.status = http.StatusNoContent
				s.body = nil
				return c.DeleteBackupRepo("fast-meta", "primary", false)
			},
			method: http.MethodDelete,
			path:   "/api/v1/store/metadata/fast-meta/repos/primary",
		},
		{
			name: "Delete with purge",
			fn: func() error {
				s.reset()
				s.status = http.StatusNoContent
				s.body = nil
				return c.DeleteBackupRepo("fast-meta", "primary", true)
			},
			method: http.MethodDelete,
			path:   "/api/v1/store/metadata/fast-meta/repos/primary?purge_archives=true",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.fn(); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if s.calls[0].Method != tc.method {
				t.Errorf("method = %q, want %q", s.calls[0].Method, tc.method)
			}
			if s.calls[0].Path != tc.path {
				t.Errorf("path = %q, want %q", s.calls[0].Path, tc.path)
			}
		})
	}
}
