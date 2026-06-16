package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestInstruments_RecordRequest(t *testing.T) {
	m := New("t", "c")
	m.RecordRequest("nfs", "READ", "ok", 5*time.Millisecond)
	m.RecordRequest("nfs", "READ", "error", time.Millisecond)
	m.RecordRequest("smb", "Create", "ok", time.Millisecond)

	expected := `
# HELP dittofs_adapter_requests_total Protocol requests handled, by protocol, operation, and status (ok|error).
# TYPE dittofs_adapter_requests_total counter
dittofs_adapter_requests_total{op="Create",protocol="smb",status="ok"} 1
dittofs_adapter_requests_total{op="READ",protocol="nfs",status="error"} 1
dittofs_adapter_requests_total{op="READ",protocol="nfs",status="ok"} 1
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(expected), "dittofs_adapter_requests_total"); err != nil {
		t.Fatalf("requests_total mismatch: %v", err)
	}
	if got := testutil.CollectAndCount(m.Registry(), "dittofs_adapter_request_duration_seconds"); got == 0 {
		t.Fatal("expected request_duration_seconds series")
	}
}

func TestInstruments_ConnAndAuth(t *testing.T) {
	m := New("t", "c")
	m.RecordConnAccepted("nfs")
	m.RecordConnAccepted("nfs")
	m.RecordConnClosed("nfs")
	m.RecordAuth("smb", "ntlm", false)
	m.RecordAuth("smb", "ntlm", true)

	expected := `
# HELP dittofs_adapter_connections_total Connections accepted since process start, by protocol.
# TYPE dittofs_adapter_connections_total counter
dittofs_adapter_connections_total{protocol="nfs"} 2
# HELP dittofs_auth_attempts_total Authentication attempts, by protocol and mechanism (sys|krb5|ntlm).
# TYPE dittofs_auth_attempts_total counter
dittofs_auth_attempts_total{mechanism="ntlm",protocol="smb"} 2
# HELP dittofs_auth_failures_total Failed authentication attempts, by protocol and mechanism (sys|krb5|ntlm).
# TYPE dittofs_auth_failures_total counter
dittofs_auth_failures_total{mechanism="ntlm",protocol="smb"} 1
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(expected),
		"dittofs_adapter_connections_total", "dittofs_auth_attempts_total", "dittofs_auth_failures_total"); err != nil {
		t.Fatalf("conn/auth mismatch: %v", err)
	}
}

func TestInstruments_NilSafe(t *testing.T) {
	var m *Metrics
	// Must not panic on nil receiver.
	m.RecordRequest("nfs", "READ", "ok", time.Millisecond)
	m.RecordConnAccepted("nfs")
	m.RecordConnClosed("nfs")
	m.RecordAuth("nfs", "krb5", false)
}
