package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/marmos91/dittofs/pkg/controlplane/runtime/trash"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// fakeTrashService is a minimal trashService test double. It records the share
// names it is called with and serves a mutable in-memory bin so List/Empty/
// Status observe each other (mirroring the real service's behavior across a
// recycle -> list -> empty flow). Error fields, when set, short-circuit the
// corresponding method.
type fakeTrashService struct {
	entries    []trash.Entry
	enabled    bool
	lastShare  string
	listErr    error
	restoreErr error
	emptyErr   error
	statusErr  error
	restored   *restoreCall
}

type restoreCall struct {
	binPath string
	dest    string
}

func (f *fakeTrashService) List(_ *metadata.AuthContext, shareName string) ([]trash.Entry, error) {
	f.lastShare = shareName
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.entries, nil
}

func (f *fakeTrashService) Restore(_ *metadata.AuthContext, shareName, binPath, dest string) error {
	f.lastShare = shareName
	if f.restoreErr != nil {
		return f.restoreErr
	}
	f.restored = &restoreCall{binPath: binPath, dest: dest}
	return nil
}

func (f *fakeTrashService) Empty(_ *metadata.AuthContext, shareName string, _ bool) (int, error) {
	f.lastShare = shareName
	if f.emptyErr != nil {
		return 0, f.emptyErr
	}
	n := len(f.entries)
	f.entries = nil
	return n, nil
}

func (f *fakeTrashService) Status(_ *metadata.AuthContext, shareName string) (*trash.Status, error) {
	f.lastShare = shareName
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	st := &trash.Status{Enabled: f.enabled, ItemCount: len(f.entries)}
	for i := range f.entries {
		st.TotalBytes += f.entries[i].Size
		t := f.entries[i].DeletedAt
		if st.Oldest == nil || t.Before(*st.Oldest) {
			oldest := t
			st.Oldest = &oldest
		}
	}
	return st, nil
}

// newTrashTestRouter mounts a TrashHandler over the fake on a chi router using
// the same route shape as the production router so {name} is extracted.
func newTrashTestRouter(svc trashService) http.Handler {
	h := &TrashHandler{svc: svc}
	r := chi.NewRouter()
	r.Route("/shares/{name}/trash", func(r chi.Router) {
		r.Get("/", h.List)
		r.Post("/restore", h.Restore)
		r.Post("/empty", h.Empty)
		r.Get("/status", h.Status)
	})
	return r
}

func TestTrashHandler_ListEmptyStatusFlow(t *testing.T) {
	deletedAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	fake := &fakeTrashService{
		enabled: true,
		entries: []trash.Entry{{
			BinPath:      "doc.txt",
			OriginalPath: "doc.txt",
			DeletedBy:    "alice",
			DeletedAt:    deletedAt,
			Size:         42,
		}},
	}
	router := newTrashTestRouter(fake)

	// GET /trash -> 200 with the single entry.
	{
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/shares/data/trash/", nil)
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("list: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var got []trash.Entry
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("list: decode body: %v", err)
		}
		if len(got) != 1 || got[0].BinPath != "doc.txt" || got[0].Size != 42 {
			t.Fatalf("list: unexpected entries: %+v", got)
		}
		// The handler normalizes the URL share name (leading slash) before
		// threading it to the service.
		if fake.lastShare != "/data" {
			t.Fatalf("list: lastShare = %q, want %q", fake.lastShare, "/data")
		}
	}

	// GET /trash/status -> 200, enabled, one item, total 42, oldest set.
	{
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/shares/data/trash/status", nil)
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status(pre): status = %d, want 200", rec.Code)
		}
		var st trash.Status
		if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
			t.Fatalf("status(pre): decode body: %v", err)
		}
		if !st.Enabled || st.ItemCount != 1 || st.TotalBytes != 42 {
			t.Fatalf("status(pre): unexpected %+v", st)
		}
		if st.Oldest == nil || !st.Oldest.Equal(deletedAt) {
			t.Fatalf("status(pre): oldest = %v, want %v", st.Oldest, deletedAt)
		}
	}

	// POST /trash/empty -> 200 {"removed":1}.
	{
		body := bytes.NewBufferString(`{"force":true}`)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/shares/data/trash/empty", body)
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("empty: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var resp emptyTrashResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("empty: decode body: %v", err)
		}
		if resp.Removed != 1 {
			t.Fatalf("empty: removed = %d, want 1", resp.Removed)
		}
	}

	// GET /trash/status -> now empty.
	{
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/shares/data/trash/status", nil)
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status(post): status = %d, want 200", rec.Code)
		}
		var st trash.Status
		if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
			t.Fatalf("status(post): decode body: %v", err)
		}
		if st.ItemCount != 0 || st.TotalBytes != 0 || st.Oldest != nil {
			t.Fatalf("status(post): expected empty bin, got %+v", st)
		}
	}
}

func TestTrashHandler_Restore(t *testing.T) {
	fake := &fakeTrashService{}
	router := newTrashTestRouter(fake)

	// Success -> 204, args threaded.
	{
		body := bytes.NewBufferString(`{"bin_path":"doc.txt","to":"restored/doc.txt"}`)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/shares/data/trash/restore", body)
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusNoContent {
			t.Fatalf("restore: status = %d, want 204; body=%s", rec.Code, rec.Body.String())
		}
		if fake.restored == nil || fake.restored.binPath != "doc.txt" || fake.restored.dest != "restored/doc.txt" {
			t.Fatalf("restore: unexpected call %+v", fake.restored)
		}
	}

	// Missing bin_path -> 400.
	{
		body := bytes.NewBufferString(`{}`)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/shares/data/trash/restore", body)
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("restore(no bin_path): status = %d, want 400", rec.Code)
		}
	}
}

func TestTrashHandler_ErrorMapping(t *testing.T) {
	notFound := &metadata.StoreError{Code: metadata.ErrNotFound, Message: "unknown share"}
	conflict := &metadata.StoreError{Code: metadata.ErrAlreadyExists, Message: "destination exists"}

	tests := []struct {
		name   string
		method string
		path   string
		body   string
		setup  func(*fakeTrashService)
		want   int
	}{
		{
			name:   "list unknown share -> 404",
			method: http.MethodGet,
			path:   "/shares/nope/trash/",
			setup:  func(f *fakeTrashService) { f.listErr = notFound },
			want:   http.StatusNotFound,
		},
		{
			name:   "restore conflict -> 409",
			method: http.MethodPost,
			path:   "/shares/data/trash/restore",
			body:   `{"bin_path":"doc.txt"}`,
			setup:  func(f *fakeTrashService) { f.restoreErr = conflict },
			want:   http.StatusConflict,
		},
		{
			name:   "restore unknown entry -> 404",
			method: http.MethodPost,
			path:   "/shares/data/trash/restore",
			body:   `{"bin_path":"gone.txt"}`,
			setup:  func(f *fakeTrashService) { f.restoreErr = notFound },
			want:   http.StatusNotFound,
		},
		{
			name:   "status unknown share -> 404",
			method: http.MethodGet,
			path:   "/shares/nope/trash/status",
			setup:  func(f *fakeTrashService) { f.statusErr = notFound },
			want:   http.StatusNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeTrashService{}
			tc.setup(fake)
			router := newTrashTestRouter(fake)

			var bodyReader *strings.Reader
			if tc.body != "" {
				bodyReader = strings.NewReader(tc.body)
			} else {
				bodyReader = strings.NewReader("")
			}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, tc.path, bodyReader)
			router.ServeHTTP(rec, req)

			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}
