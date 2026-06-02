package block

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
	"github.com/marmos91/dittofs/pkg/block/engine"
)

// auditServer is a recording stub for the refcount audit endpoint.
type auditServer struct {
	*httptest.Server
	lastMethod string
	lastPath   string
	result     *engine.AuditRefcountsResult
	status     int
}

func newAuditServer(t *testing.T) *auditServer {
	t.Helper()
	s := &auditServer{status: http.StatusOK}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.lastMethod = r.Method
		s.lastPath = r.URL.Path

		w.Header().Set("Content-Type", "application/json")
		if s.status >= 400 {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(s.status)
			_, _ = io.WriteString(w, `{"type":"about:blank","title":"Not Found","status":404,"detail":"share not found"}`)
			return
		}
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/audit/refcounts") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(apiclient.BlockStoreAuditResult{Result: s.result})
	}))
	return s
}

func withAuditTestServer(t *testing.T, url string) {
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

// captureStdoutAudit captures os.Stdout for the duration of fn().
// Mirrors captureStdoutBlock in gc_test.go but kept local so audit tests
// don't take a hard dependency on a sibling test's helper visibility.
func captureStdoutAudit(t *testing.T, fn func()) string {
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

// TestAuditCmd_CallsClient_AndPrintsSummary asserts the CLI hits the
// per-share audit endpoint and renders the operator-visible fields.
func TestAuditCmd_CallsClient_AndPrintsSummary(t *testing.T) {
	s := newAuditServer(t)
	defer s.Close()
	now := time.Now().UTC().Truncate(time.Second)
	s.result = &engine.AuditRefcountsResult{
		Share:         "myshare",
		StartedAt:     now,
		CompletedAt:   now.Add(time.Second),
		DurationMS:    1000,
		TotalFiles:    3,
		TotalRefs:     10,
		TotalRefCount: 10,
		Delta:         0,
	}
	withAuditTestServer(t, s.URL)

	out := captureStdoutAudit(t, func() {
		if err := runAuditRefcounts(auditRefcountsCmd, []string{"myshare"}); err != nil {
			t.Fatalf("runAuditRefcounts: %v", err)
		}
	})

	if s.lastMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", s.lastMethod)
	}
	if s.lastPath != "/api/v1/shares/myshare/audit/refcounts" {
		t.Errorf("path = %q, want /api/v1/shares/myshare/audit/refcounts", s.lastPath)
	}
	for _, frag := range []string{"Share", "myshare", "Total Files", "3", "Total Refs", "10", "Delta", "0"} {
		if !strings.Contains(out, frag) {
			t.Errorf("stdout missing %q, got %q", frag, out)
		}
	}
	if strings.Contains(out, "INV-02 violation") {
		t.Errorf("delta=0 must not surface INV-02 violation banner; got %q", out)
	}
}

// TestAuditCmd_DriftSurfacesViolation asserts a non-zero delta appends
// the violation banner to the table output AND returns a non-nil error so
// cobra exits non-zero (scripts can gate on `audit-refcounts || alert`).
func TestAuditCmd_DriftSurfacesViolation(t *testing.T) {
	s := newAuditServer(t)
	defer s.Close()
	now := time.Now().UTC().Truncate(time.Second)
	s.result = &engine.AuditRefcountsResult{
		Share:         "myshare",
		StartedAt:     now,
		CompletedAt:   now.Add(time.Second),
		TotalFiles:    3,
		TotalRefs:     10,
		TotalRefCount: 15,
		Delta:         -5,
	}
	withAuditTestServer(t, s.URL)

	var runErr error
	out := captureStdoutAudit(t, func() {
		runErr = runAuditRefcounts(auditRefcountsCmd, []string{"myshare"})
	})

	if runErr == nil {
		t.Fatal("non-zero delta must return a non-nil error (non-zero exit)")
	}
	if !strings.Contains(runErr.Error(), "delta=-5") {
		t.Errorf("error must include the delta value; got %v", runErr)
	}
	if !strings.Contains(out, "INV-02 violation") {
		t.Errorf("non-zero delta must surface INV-02 violation banner; got %q", out)
	}
	if !strings.Contains(out, "delta=-5") {
		t.Errorf("violation banner must include delta value; got %q", out)
	}
}

// TestAuditCmd_DriftExitsNonZeroAcrossFormats asserts a non-zero delta
// returns a non-nil error in EVERY output format — the exit-0-on-failure
// class fix. Without it, `audit-refcounts -o json || alert` never fires on
// detected corruption. The JSON/YAML body is still emitted for observability.
func TestAuditCmd_DriftExitsNonZeroAcrossFormats(t *testing.T) {
	for _, format := range []string{"json", "yaml"} {
		t.Run(format, func(t *testing.T) {
			s := newAuditServer(t)
			defer s.Close()
			s.result = &engine.AuditRefcountsResult{
				Share:         "myshare",
				TotalRefs:     10,
				TotalRefCount: 7,
				Delta:         3,
			}
			withAuditTestServer(t, s.URL)
			cmdutil.Flags.Output = format

			var runErr error
			out := captureStdoutAudit(t, func() {
				runErr = runAuditRefcounts(auditRefcountsCmd, []string{"myshare"})
			})

			if runErr == nil {
				t.Fatalf("%s: non-zero delta must return a non-nil error", format)
			}
			if !strings.Contains(runErr.Error(), "delta=3") {
				t.Errorf("%s: error must include the delta value; got %v", format, runErr)
			}
			// Body is still emitted so machine consumers get the full result.
			if !strings.Contains(out, "myshare") {
				t.Errorf("%s: result body must still be emitted; got %q", format, out)
			}
		})
	}
}

// TestAuditCmd_NoArg_Errors confirms cobra.ExactArgs(1) rejects bare invocations.
func TestAuditCmd_NoArg_Errors(t *testing.T) {
	err := auditRefcountsCmd.Args(auditRefcountsCmd, []string{})
	if err == nil {
		t.Fatal("expected error on zero args, got nil")
	}
}

// TestAuditCmd_Registered confirms the subcommand is registered on the
// block parent so `dfsctl store block --help` lists it. Guard against a
// future refactor that drops the AddCommand call.
func TestAuditCmd_Registered(t *testing.T) {
	found := false
	for _, sub := range Cmd.Commands() {
		if sub == auditRefcountsCmd {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("auditRefcountsCmd not registered on block.Cmd")
	}
}
