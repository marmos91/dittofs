package trash

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
)

// trashServer is a recording stub answering the four trash endpoints,
// capturing the path/method and decoded request bodies for assertions.
// Mirrors newGCServer in cmd/dfsctl/commands/store/block/gc_test.go.
type trashServer struct {
	*httptest.Server
	lastMethod  string
	lastPath    string
	lastBinPath string
	lastTo      string
	lastForce   bool
	entries     []apiclient.TrashEntry
	status      *apiclient.TrashStatus
	removed     int
	httpStatus  int
}

func newTrashServer(t *testing.T) *trashServer {
	t.Helper()
	s := &trashServer{httpStatus: http.StatusOK}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.lastMethod = r.Method
		s.lastPath = r.URL.Path

		w.Header().Set("Content-Type", "application/json")
		if s.httpStatus >= 400 {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(s.httpStatus)
			_, _ = io.WriteString(w, `{"type":"about:blank","title":"Conflict","status":409,"detail":"destination exists"}`)
			return
		}

		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/trash"):
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(s.entries)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/trash/status"):
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(s.status)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/trash/restore"):
			body, _ := io.ReadAll(r.Body)
			var req struct {
				BinPath string `json:"bin_path"`
				To      string `json:"to"`
			}
			_ = json.Unmarshal(body, &req)
			s.lastBinPath = req.BinPath
			s.lastTo = req.To
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/trash/empty"):
			body, _ := io.ReadAll(r.Body)
			var req struct {
				Force bool `json:"force"`
			}
			_ = json.Unmarshal(body, &req)
			s.lastForce = req.Force
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]int{"removed": s.removed})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return s
}

// withTrashTestServer points cmdutil.Flags at the stub server.
func withTrashTestServer(t *testing.T, url string) {
	t.Helper()
	origServer, origToken, origOutput := cmdutil.Flags.ServerURL, cmdutil.Flags.Token, cmdutil.Flags.Output
	cmdutil.Flags.ServerURL = url
	cmdutil.Flags.Token = "test-token"
	cmdutil.Flags.Output = "table"
	t.Cleanup(func() {
		cmdutil.Flags.ServerURL = origServer
		cmdutil.Flags.Token = origToken
		cmdutil.Flags.Output = origOutput
	})
}

// captureStdout redirects os.Stdout for the duration of fn and returns what
// was written.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	_ = w.Close()
	os.Stdout = orig
	return <-done
}

// TestTrashListCmd_CallsClient_AndRendersEntries asserts list hits
// GET /trash and renders the entry fields an operator reads.
func TestTrashListCmd_CallsClient_AndRendersEntries(t *testing.T) {
	s := newTrashServer(t)
	defer s.Close()
	s.entries = []apiclient.TrashEntry{
		{
			BinPath:      "#recycle/2026-06-01/report.txt",
			OriginalPath: "/docs/report.txt",
			DeletedBy:    "alice",
			DeletedAt:    time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC),
			Size:         2048,
			IsDir:        false,
		},
		{
			BinPath:      "#recycle/2026-06-01/olddir",
			OriginalPath: "/work/olddir",
			DeletedBy:    "bob",
			DeletedAt:    time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC),
			IsDir:        true,
		},
	}
	withTrashTestServer(t, s.URL)

	out := captureStdout(t, func() {
		if err := runTrashList(listCmd, []string{"myshare"}); err != nil {
			t.Fatalf("runTrashList: %v", err)
		}
	})

	if s.lastMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", s.lastMethod)
	}
	if s.lastPath != "/api/v1/shares/myshare/trash" {
		t.Errorf("path = %q, want /api/v1/shares/myshare/trash", s.lastPath)
	}
	for _, frag := range []string{"PATH", "ORIGINAL", "DELETED BY", "report.txt", "/docs/report.txt", "alice", "olddir", "bob", "dir", "file"} {
		if !strings.Contains(out, frag) {
			t.Errorf("stdout missing %q, got %q", frag, out)
		}
	}
}

// TestTrashListCmd_Empty renders the friendly empty message.
func TestTrashListCmd_Empty(t *testing.T) {
	s := newTrashServer(t)
	defer s.Close()
	s.entries = []apiclient.TrashEntry{}
	withTrashTestServer(t, s.URL)

	out := captureStdout(t, func() {
		if err := runTrashList(listCmd, []string{"myshare"}); err != nil {
			t.Fatalf("runTrashList: %v", err)
		}
	})
	if !strings.Contains(out, "Trash is empty") {
		t.Errorf("expected empty message, got %q", out)
	}
}

// TestTrashRestoreCmd_PostsBinPathAndTo asserts restore POSTs the bin_path
// and --to and prints a confirmation.
func TestTrashRestoreCmd_PostsBinPathAndTo(t *testing.T) {
	s := newTrashServer(t)
	defer s.Close()
	withTrashTestServer(t, s.URL)

	out := captureStdout(t, func() {
		if err := restoreCmd.Flags().Set("to", "/restored/report.txt"); err != nil {
			t.Fatalf("set to: %v", err)
		}
		t.Cleanup(func() { _ = restoreCmd.Flags().Set("to", "") })
		if err := runTrashRestore(restoreCmd, []string{"myshare", "#recycle/2026-06-01/report.txt"}); err != nil {
			t.Fatalf("runTrashRestore: %v", err)
		}
	})

	if s.lastMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", s.lastMethod)
	}
	if s.lastPath != "/api/v1/shares/myshare/trash/restore" {
		t.Errorf("path = %q, want /api/v1/shares/myshare/trash/restore", s.lastPath)
	}
	if s.lastBinPath != "#recycle/2026-06-01/report.txt" {
		t.Errorf("bin_path = %q", s.lastBinPath)
	}
	if s.lastTo != "/restored/report.txt" {
		t.Errorf("to = %q, want /restored/report.txt", s.lastTo)
	}
	if !strings.Contains(out, "Restored") {
		t.Errorf("expected confirmation, got %q", out)
	}
}

// TestTrashRestoreCmd_Conflict maps a 409 to a clear --to hint.
func TestTrashRestoreCmd_Conflict(t *testing.T) {
	s := newTrashServer(t)
	defer s.Close()
	s.httpStatus = http.StatusConflict
	withTrashTestServer(t, s.URL)

	_ = restoreCmd.Flags().Set("to", "")
	err := runTrashRestore(restoreCmd, []string{"myshare", "#recycle/x"})
	if err == nil {
		t.Fatal("expected conflict error, got nil")
	}
	if !strings.Contains(err.Error(), "--to") {
		t.Errorf("error should hint at --to, got %v", err)
	}
}

// TestTrashEmptyCmd_PostsForce asserts empty POSTs the force flag and prints
// the removed count.
func TestTrashEmptyCmd_PostsForce(t *testing.T) {
	s := newTrashServer(t)
	defer s.Close()
	s.removed = 7
	withTrashTestServer(t, s.URL)

	out := captureStdout(t, func() {
		if err := emptyCmd.Flags().Set("force", "true"); err != nil {
			t.Fatalf("set force: %v", err)
		}
		t.Cleanup(func() { _ = emptyCmd.Flags().Set("force", "false") })
		if err := runTrashEmpty(emptyCmd, []string{"myshare"}); err != nil {
			t.Fatalf("runTrashEmpty: %v", err)
		}
	})

	if s.lastMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", s.lastMethod)
	}
	if s.lastPath != "/api/v1/shares/myshare/trash/empty" {
		t.Errorf("path = %q, want /api/v1/shares/myshare/trash/empty", s.lastPath)
	}
	if !s.lastForce {
		t.Error("force flag should be true on request body")
	}
	if !strings.Contains(out, "Removed 7 item(s)") {
		t.Errorf("expected removed count, got %q", out)
	}
}

// TestTrashStatusCmd_Renders asserts status hits GET /trash/status and
// renders the roll-up fields.
func TestTrashStatusCmd_Renders(t *testing.T) {
	s := newTrashServer(t)
	defer s.Close()
	oldest := time.Date(2026, 5, 30, 8, 0, 0, 0, time.UTC)
	s.status = &apiclient.TrashStatus{
		Enabled:    true,
		ItemCount:  3,
		TotalBytes: 4096,
		Oldest:     &oldest,
	}
	withTrashTestServer(t, s.URL)

	out := captureStdout(t, func() {
		if err := runTrashStatus(statusCmd, []string{"myshare"}); err != nil {
			t.Fatalf("runTrashStatus: %v", err)
		}
	})

	if s.lastMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", s.lastMethod)
	}
	if s.lastPath != "/api/v1/shares/myshare/trash/status" {
		t.Errorf("path = %q, want /api/v1/shares/myshare/trash/status", s.lastPath)
	}
	for _, frag := range []string{"Enabled", "true", "Items", "3", "Total Size", "Oldest", "2026-05-30"} {
		if !strings.Contains(out, frag) {
			t.Errorf("stdout missing %q, got %q", frag, out)
		}
	}
}

// TestTrashStatusCmd_NoOldest renders "-" when the bin is empty.
func TestTrashStatusCmd_NoOldest(t *testing.T) {
	s := newTrashServer(t)
	defer s.Close()
	s.status = &apiclient.TrashStatus{Enabled: true}
	withTrashTestServer(t, s.URL)

	out := captureStdout(t, func() {
		if err := runTrashStatus(statusCmd, []string{"myshare"}); err != nil {
			t.Fatalf("runTrashStatus: %v", err)
		}
	})
	if !strings.Contains(out, "Oldest") || !strings.Contains(out, "-") {
		t.Errorf("expected Oldest placeholder, got %q", out)
	}
}

// TestTrashCmds_ExactArgs confirms the single-share commands reject bare
// invocations and restore requires two args.
func TestTrashCmds_ExactArgs(t *testing.T) {
	if err := listCmd.Args(listCmd, []string{}); err == nil {
		t.Error("list: expected error on zero args")
	}
	if err := emptyCmd.Args(emptyCmd, []string{}); err == nil {
		t.Error("empty: expected error on zero args")
	}
	if err := statusCmd.Args(statusCmd, []string{}); err == nil {
		t.Error("status: expected error on zero args")
	}
	if err := restoreCmd.Args(restoreCmd, []string{"onlyshare"}); err == nil {
		t.Error("restore: expected error on one arg (needs share + bin-path)")
	}
}
