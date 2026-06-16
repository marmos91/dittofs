package handlers

// Tests for SMB authentication metrics (PR-2b of #1188): SESSION_SETUP records
// one dittofs_auth_attempts_total{protocol=smb,mechanism} per terminal verdict,
// plus a dittofs_auth_failures_total on failure. Mechanism is bounded to
// {ntlm,krb5}.

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/auth"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metrics"
)

// newMetricsHandler builds a Handler whose Registry is a runtime with a real,
// owned metrics registry the test can scrape, backed by an in-memory store.
func newMetricsHandler(t *testing.T) (*Handler, *metrics.Metrics) {
	t.Helper()
	h := NewHandler()
	h.NtlmEnabled = true
	rt := runtime.New(newInMemoryStoreForTest(t))
	m := metrics.New("test", "test")
	rt.SetMetrics(m)
	h.Registry = rt
	return h, m
}

func TestSessionSetup_RecordsKerberosFailure(t *testing.T) {
	h, m := newMetricsHandler(t)
	// No KerberosService configured: handleKerberosAuth returns LOGON_FAILURE,
	// which the auth recorder counts as one failed krb5 attempt.
	ctx := NewSMBHandlerContext(context.Background(), "127.0.0.1:1", 0, 0, 1)
	res, err := h.handleKerberosAuth(ctx, []byte{0x01, 0x02}, &auth.ParsedToken{})
	if err != nil {
		t.Fatalf("handleKerberosAuth returned error: %v", err)
	}
	if res == nil || res.Status != types.StatusLogonFailure {
		t.Fatalf("expected LOGON_FAILURE result, got %+v", res)
	}

	if got := counterValue(t, m, "dittofs_auth_attempts_total", "smb", "krb5"); got != 1 {
		t.Fatalf("krb5 attempts = %v, want 1", got)
	}
	if got := counterValue(t, m, "dittofs_auth_failures_total", "smb", "krb5"); got != 1 {
		t.Fatalf("krb5 failures = %v, want 1", got)
	}
}

func TestSessionSetup_RecordsKerberosBindFailure(t *testing.T) {
	h, m := newMetricsHandler(t)
	// completeKerberosBind is the single-shot Kerberos channel-bind verdict. With
	// no KerberosService it returns LOGON_FAILURE, recorded as one failed krb5
	// attempt — the bind path must be instrumented just like handleKerberosAuth.
	const sessionID = uint64(0xabc)
	sess := h.CreateSessionWithUser(sessionID, "127.0.0.1:1", &models.User{Username: "alice", Enabled: true}, "")
	ctx := NewSMBHandlerContext(context.Background(), "127.0.0.1:1", sessionID, 0, 1)

	res, err := h.completeKerberosBind(ctx, sess, &auth.ParsedToken{})
	if err != nil {
		t.Fatalf("completeKerberosBind returned error: %v", err)
	}
	if res == nil || res.Status != types.StatusLogonFailure {
		t.Fatalf("expected LOGON_FAILURE result, got %+v", res)
	}

	if got := counterValue(t, m, "dittofs_auth_attempts_total", "smb", "krb5"); got != 1 {
		t.Fatalf("krb5 bind attempts = %v, want 1", got)
	}
	if got := counterValue(t, m, "dittofs_auth_failures_total", "smb", "krb5"); got != 1 {
		t.Fatalf("krb5 bind failures = %v, want 1", got)
	}
}

func TestSessionSetup_RecordsNTLMFailure(t *testing.T) {
	h, m := newMetricsHandler(t)
	// Seed a pending NTLM handshake so completeNTLMAuth treats this as a
	// terminal TYPE_3 verdict. The user does not exist in the store → the
	// userStore lookup fails → LOGON_FAILURE → one failed ntlm attempt.
	const sessionID, connID = uint64(7), uint64(3)
	h.StorePendingAuth(&PendingAuth{SessionID: sessionID, ConnID: connID})

	ctx := NewSMBHandlerContext(context.Background(), "127.0.0.1:1", sessionID, 0, 1)
	ctx.ConnID = connID

	authMsg := buildNTLMAuthenticateForTest("ghost", "", nil)
	res, err := h.completeNTLMAuth(ctx, authMsg)
	if err != nil {
		t.Fatalf("completeNTLMAuth returned error: %v", err)
	}
	if res == nil || res.Status != types.StatusLogonFailure {
		t.Fatalf("expected LOGON_FAILURE result, got %+v", res)
	}

	if got := counterValue(t, m, "dittofs_auth_attempts_total", "smb", "ntlm"); got != 1 {
		t.Fatalf("ntlm attempts = %v, want 1", got)
	}
	if got := counterValue(t, m, "dittofs_auth_failures_total", "smb", "ntlm"); got != 1 {
		t.Fatalf("ntlm failures = %v, want 1", got)
	}
}

// counterValue scrapes the named counter and returns the value of the sample
// matching {protocol,mechanism}, or -1 when no such sample exists.
func counterValue(t *testing.T, m *metrics.Metrics, name, protocol, mechanism string) float64 {
	t.Helper()
	mfs, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, mc := range mf.GetMetric() {
			var p, mech string
			for _, l := range mc.GetLabel() {
				switch l.GetName() {
				case "protocol":
					p = l.GetValue()
				case "mechanism":
					mech = l.GetValue()
				}
			}
			if p == protocol && mech == mechanism {
				return mc.GetCounter().GetValue()
			}
		}
	}
	return -1
}
