package share

import (
	"net/http"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
)

func TestEnableCmd_CallsClient_AndPrintsSuccess(t *testing.T) {
	s := newShareActionServer(t)
	defer s.Close()
	s.body = apiclient.Share{ID: "s1", Name: "/alice", Enabled: true}
	withTestServer(t, s.URL)

	out := captureStdout(t, func() {
		if err := runEnable(enableCmd, []string{"alice"}); err != nil {
			t.Fatalf("runEnable: %v", err)
		}
	})

	if s.lastMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", s.lastMethod)
	}
	if s.lastPath != "/api/v1/shares/alice/enable" {
		t.Errorf("path = %q, want /api/v1/shares/alice/enable", s.lastPath)
	}
	if !strings.Contains(out, "Share alice enabled.") {
		t.Errorf("stdout missing success message, got %q", out)
	}
}

func TestEnableCmd_JSONMode_EmitsShare(t *testing.T) {
	s := newShareActionServer(t)
	defer s.Close()
	s.body = apiclient.Share{ID: "s1", Name: "/alice", Enabled: true}
	withTestServer(t, s.URL)
	cmdutil.Flags.Output = "json"

	out := captureStdout(t, func() {
		if err := runEnable(enableCmd, []string{"alice"}); err != nil {
			t.Fatalf("runEnable: %v", err)
		}
	})

	if !strings.Contains(out, `"enabled": true`) && !strings.Contains(out, `"enabled":true`) {
		t.Errorf("json output missing enabled:true, got %q", out)
	}
}

func TestEnableCmd_NoArg_Errors(t *testing.T) {
	enableCmd.SetArgs([]string{})
	err := enableCmd.Args(enableCmd, []string{})
	if err == nil {
		t.Fatal("expected error on zero args, got nil")
	}
	if !strings.Contains(err.Error(), "accepts 1 arg") && !strings.Contains(err.Error(), "requires") {
		t.Errorf("error should mention arg count, got %v", err)
	}
}

func TestEnableCmd_NotFound_Exits1(t *testing.T) {
	s := newShareActionServer(t)
	defer s.Close()
	s.status = http.StatusNotFound
	withTestServer(t, s.URL)

	err := runEnable(enableCmd, []string{"nonexistent"})
	if err == nil {
		t.Fatal("expected error on 404, got nil")
	}
	if !strings.Contains(err.Error(), "failed to enable share") {
		t.Errorf("error should wrap with 'failed to enable share', got %v", err)
	}
}
