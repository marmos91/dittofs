package share

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
)

// newShareActionServer returns an httptest server that answers a single
// POST /api/v1/shares/{name}/{action} request. It captures the path + method
// so tests can assert both and returns the configured body + status.
type shareActionServer struct {
	*httptest.Server
	lastMethod string
	lastPath   string
	body       apiclient.Share
	status     int
}

func newShareActionServer(t *testing.T) *shareActionServer {
	t.Helper()
	s := &shareActionServer{status: http.StatusOK}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.lastMethod = r.Method
		s.lastPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(s.status)
		if s.status >= 400 {
			_, _ = io.WriteString(w, `{"type":"urn:dittofs:problem:test","title":"test","status":`+itoa(s.status)+`,"detail":"test-error"}`)
			return
		}
		_ = json.NewEncoder(w).Encode(s.body)
	}))
	return s
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// withTestServer points cmdutil.Flags at the stub server so
// GetAuthenticatedClient() short-circuits the credential store.
func withTestServer(t *testing.T, url string) {
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

// captureStdout redirects os.Stdout for the duration of fn and returns the
// bytes written. PrintSuccess writes directly to os.Stdout, so we need the
// process-level swap rather than an io.Writer argument.
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

func TestDisableCmd_CallsClient_AndPrintsSuccess(t *testing.T) {
	s := newShareActionServer(t)
	defer s.Close()
	s.body = apiclient.Share{ID: "s1", Name: "/alice", Enabled: false}
	withTestServer(t, s.URL)

	out := captureStdout(t, func() {
		if err := runDisable(disableCmd, []string{"alice"}); err != nil {
			t.Fatalf("runDisable: %v", err)
		}
	})

	if s.lastMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", s.lastMethod)
	}
	if s.lastPath != "/api/v1/shares/alice/disable" {
		t.Errorf("path = %q, want /api/v1/shares/alice/disable", s.lastPath)
	}
	if !strings.Contains(out, "Share alice disabled.") {
		t.Errorf("stdout missing success message, got %q", out)
	}
}

func TestDisableCmd_JSONMode_EmitsShare(t *testing.T) {
	s := newShareActionServer(t)
	defer s.Close()
	s.body = apiclient.Share{ID: "s1", Name: "/alice", Enabled: false}
	withTestServer(t, s.URL)
	cmdutil.Flags.Output = "json"

	out := captureStdout(t, func() {
		if err := runDisable(disableCmd, []string{"alice"}); err != nil {
			t.Fatalf("runDisable: %v", err)
		}
	})

	if !strings.Contains(out, `"enabled": false`) && !strings.Contains(out, `"enabled":false`) {
		t.Errorf("json output missing enabled:false, got %q", out)
	}
	if !strings.Contains(out, `"name"`) {
		t.Errorf("json output missing share name, got %q", out)
	}
}

func TestDisableCmd_NoArg_Errors(t *testing.T) {
	// Cobra's cobra.ExactArgs(1) enforces the count at Args validation; the
	// RunE itself assumes one positional. Invoke the command through the tree
	// to exercise Args.
	disableCmd.SetArgs([]string{})
	err := disableCmd.Args(disableCmd, []string{})
	if err == nil {
		t.Fatal("expected error on zero args, got nil")
	}
	if !strings.Contains(err.Error(), "accepts 1 arg") && !strings.Contains(err.Error(), "requires") {
		t.Errorf("error should mention arg count, got %v", err)
	}
}

func TestDisableCmd_NotFound_Exits1(t *testing.T) {
	s := newShareActionServer(t)
	defer s.Close()
	s.status = http.StatusNotFound
	withTestServer(t, s.URL)

	err := runDisable(disableCmd, []string{"nonexistent"})
	if err == nil {
		t.Fatal("expected error on 404, got nil")
	}
	if !strings.Contains(err.Error(), "failed to disable share") {
		t.Errorf("error should wrap with 'failed to disable share', got %v", err)
	}
}
