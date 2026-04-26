package block

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/marmos91/dittofs/pkg/blockstore/engine"
)

// gcServer is a recording stub that answers either the GC trigger or
// the gc-status endpoint, capturing the path/method and the decoded
// dry_run flag for assertions.
type gcServer struct {
	*httptest.Server
	lastMethod string
	lastPath   string
	lastDryRun bool
	gcStats    *engine.GCStats
	summary    engine.GCRunSummary
	status     int
}

func newGCServer(t *testing.T) *gcServer {
	t.Helper()
	s := &gcServer{status: http.StatusOK}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.lastMethod = r.Method
		s.lastPath = r.URL.Path

		w.Header().Set("Content-Type", "application/json")
		if s.status >= 400 {
			w.WriteHeader(s.status)
			body := `{"code":"NOT_FOUND","message":"no GC run recorded"}`
			_, _ = io.WriteString(w, body)
			return
		}

		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/gc"):
			body, _ := io.ReadAll(r.Body)
			var opts apiclient.BlockStoreGCOptions
			_ = json.Unmarshal(body, &opts)
			s.lastDryRun = opts.DryRun
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(apiclient.BlockStoreGCResult{Stats: s.gcStats})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/gc-status"):
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(s.summary)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return s
}

// withGCTestServer points cmdutil.Flags at the stub server. Mirrors
// withTestServer in the share package so tests share the same shape.
func withGCTestServer(t *testing.T, url string) {
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

func captureStdoutBlock(t *testing.T, fn func()) string {
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

// TestGCCmd_CallsClient_AndPrintsSummary asserts a non-dry-run invocation
// posts to the per-share GC endpoint with dry_run=false and renders the
// summary fields the operator actually reads.
func TestGCCmd_CallsClient_AndPrintsSummary(t *testing.T) {
	s := newGCServer(t)
	defer s.Close()
	s.gcStats = &engine.GCStats{
		RunID:        "run-1",
		HashesMarked: 11,
		ObjectsSwept: 3,
		BytesFreed:   2048,
		DurationMs:   500,
	}
	withGCTestServer(t, s.URL)

	out := captureStdoutBlock(t, func() {
		// Reset the dry-run flag from any prior test invocation.
		_ = gcCmd.Flags().Set("dry-run", "false")
		if err := runBlockStoreGC(gcCmd, []string{"myshare"}); err != nil {
			t.Fatalf("runBlockStoreGC: %v", err)
		}
	})

	if s.lastMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", s.lastMethod)
	}
	if s.lastPath != "/api/v1/shares/myshare/blockstore/gc" {
		t.Errorf("path = %q, want /api/v1/shares/myshare/blockstore/gc", s.lastPath)
	}
	if s.lastDryRun {
		t.Error("dry_run flag should be false by default")
	}
	for _, frag := range []string{"Hashes Marked", "11", "Objects Swept", "3", "Bytes Freed", "Run ID", "run-1"} {
		if !strings.Contains(out, frag) {
			t.Errorf("stdout missing %q, got %q", frag, out)
		}
	}
}

// TestGCCmd_DryRunFlag asserts --dry-run propagates to the request body
// and the candidate listing surfaces in the table output.
func TestGCCmd_DryRunFlag(t *testing.T) {
	s := newGCServer(t)
	defer s.Close()
	s.gcStats = &engine.GCStats{
		RunID:            "run-2",
		DryRun:           true,
		HashesMarked:     20,
		DryRunCandidates: []string{"cas/aa/bb/abcdef", "cas/aa/cc/123456"},
	}
	withGCTestServer(t, s.URL)

	out := captureStdoutBlock(t, func() {
		if err := gcCmd.Flags().Set("dry-run", "true"); err != nil {
			t.Fatalf("set dry-run: %v", err)
		}
		t.Cleanup(func() { _ = gcCmd.Flags().Set("dry-run", "false") })

		if err := runBlockStoreGC(gcCmd, []string{"myshare"}); err != nil {
			t.Fatalf("runBlockStoreGC: %v", err)
		}
	})

	if !s.lastDryRun {
		t.Error("dry_run flag should be true on request body")
	}
	for _, frag := range []string{"Dry-run candidates", "cas/aa/bb/abcdef", "cas/aa/cc/123456"} {
		if !strings.Contains(out, frag) {
			t.Errorf("stdout missing %q, got %q", frag, out)
		}
	}
}

// TestGCCmd_NoArg_Errors confirms cobra.ExactArgs(1) rejects bare
// invocations before RunE runs.
func TestGCCmd_NoArg_Errors(t *testing.T) {
	err := gcCmd.Args(gcCmd, []string{})
	if err == nil {
		t.Fatal("expected error on zero args, got nil")
	}
	if !strings.Contains(err.Error(), "accepts 1 arg") && !strings.Contains(err.Error(), "requires") {
		t.Errorf("error should mention arg count, got %v", err)
	}
}

// TestGCStatusCmd_PrintsSummary asserts gc-status hits the read endpoint
// and renders the summary fields.
func TestGCStatusCmd_PrintsSummary(t *testing.T) {
	s := newGCServer(t)
	defer s.Close()
	s.summary = engine.GCRunSummary{
		RunID:        "run-7",
		StartedAt:    time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC),
		CompletedAt:  time.Date(2026, 4, 25, 10, 0, 1, 0, time.UTC),
		HashesMarked: 99,
		ObjectsSwept: 5,
		BytesFreed:   8192,
		DurationMs:   1100,
	}
	withGCTestServer(t, s.URL)

	out := captureStdoutBlock(t, func() {
		if err := runBlockStoreGCStatus(gcStatusCmd, []string{"myshare"}); err != nil {
			t.Fatalf("runBlockStoreGCStatus: %v", err)
		}
	})

	if s.lastMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", s.lastMethod)
	}
	if s.lastPath != "/api/v1/shares/myshare/blockstore/gc-status" {
		t.Errorf("path = %q, want /api/v1/shares/myshare/blockstore/gc-status", s.lastPath)
	}
	for _, frag := range []string{"run-7", "Hashes Marked", "99", "Objects Swept", "5"} {
		if !strings.Contains(out, frag) {
			t.Errorf("stdout missing %q, got %q", frag, out)
		}
	}
}

// TestGCStatusCmd_NoRunYet maps a 404 from the server to the
// ErrNoGCRunYet sentinel so callers (including scripts via cobra's
// non-zero exit) can branch on the "first deploy" state.
func TestGCStatusCmd_NoRunYet(t *testing.T) {
	s := newGCServer(t)
	defer s.Close()
	s.status = http.StatusNotFound
	withGCTestServer(t, s.URL)

	err := runBlockStoreGCStatus(gcStatusCmd, []string{"myshare"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrNoGCRunYet) {
		t.Errorf("expected errors.Is(err, ErrNoGCRunYet), got %v", err)
	}
}

// TestGCStatusCmd_NoArg_Errors confirms cobra.ExactArgs(1) gates the
// gc-status command too.
func TestGCStatusCmd_NoArg_Errors(t *testing.T) {
	err := gcStatusCmd.Args(gcStatusCmd, []string{})
	if err == nil {
		t.Fatal("expected error on zero args, got nil")
	}
}

// TestGCCmd_HelpListsDryRun is a guard against a future refactor that
// removes the --dry-run flag — operators read --help to discover the
// dry-run mode (D-09 essential for first-deploy confidence).
func TestGCCmd_HelpListsDryRun(t *testing.T) {
	if f := gcCmd.Flags().Lookup("dry-run"); f == nil {
		t.Fatal("gc subcommand must declare --dry-run flag")
	}
}
