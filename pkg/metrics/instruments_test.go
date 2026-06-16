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

func TestInstruments_EvictionAndBackpressure(t *testing.T) {
	m := New("t", "c")
	m.RecordBackpressure(2 * time.Second)
	m.RecordEviction(8 * 1024 * 1024)
	m.RecordEviction(4 * 1024 * 1024)
	m.RecordEviction(0) // zero bytes: counts the eviction, adds no bytes.

	expected := `
# HELP dittofs_localstore_backpressure_total Times a write stalled waiting for the local cache to free space.
# TYPE dittofs_localstore_backpressure_total counter
dittofs_localstore_backpressure_total 1
# HELP dittofs_localstore_evicted_bytes_total Bytes reclaimed by local-cache eviction.
# TYPE dittofs_localstore_evicted_bytes_total counter
dittofs_localstore_evicted_bytes_total 1.2582912e+07
# HELP dittofs_localstore_evictions_total Local-cache CAS chunks evicted to reclaim space.
# TYPE dittofs_localstore_evictions_total counter
dittofs_localstore_evictions_total 3
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(expected),
		"dittofs_localstore_backpressure_total", "dittofs_localstore_evicted_bytes_total",
		"dittofs_localstore_evictions_total"); err != nil {
		t.Fatalf("eviction/backpressure mismatch: %v", err)
	}
	if got := testutil.CollectAndCount(m.Registry(), "dittofs_localstore_backpressure_wait_seconds"); got == 0 {
		t.Fatal("expected backpressure_wait_seconds series")
	}
}

func TestInstruments_GC(t *testing.T) {
	m := New("t", "c")
	// Two balanced start/finish pairs: gauge returns to 0.
	m.GCStarted()
	m.GCFinished("ok", 12, 96*1024*1024, 5*time.Second)
	m.GCStarted()
	m.GCFinished("error", 0, 0, time.Second)

	expected := `
# HELP dittofs_gc_freed_bytes_total Bytes freed by block-store GC.
# TYPE dittofs_gc_freed_bytes_total counter
dittofs_gc_freed_bytes_total 1.00663296e+08
# HELP dittofs_gc_running Number of block-store GC passes currently in progress (0 when idle; >1 when passes overlap). >0 indicates a pass is running.
# TYPE dittofs_gc_running gauge
dittofs_gc_running 0
# HELP dittofs_gc_runs_total Block-store GC passes completed, by result (ok|error).
# TYPE dittofs_gc_runs_total counter
dittofs_gc_runs_total{result="error"} 1
dittofs_gc_runs_total{result="ok"} 1
# HELP dittofs_gc_swept_objects_total CAS objects reaped by block-store GC.
# TYPE dittofs_gc_swept_objects_total counter
dittofs_gc_swept_objects_total 12
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(expected),
		"dittofs_gc_freed_bytes_total", "dittofs_gc_running", "dittofs_gc_runs_total",
		"dittofs_gc_swept_objects_total"); err != nil {
		t.Fatalf("gc mismatch: %v", err)
	}
	for _, name := range []string{"dittofs_gc_duration_seconds", "dittofs_gc_last_run_timestamp_seconds"} {
		if got := testutil.CollectAndCount(m.Registry(), name); got == 0 {
			t.Fatalf("expected %s series", name)
		}
	}
}

func TestInstruments_GCRunningGauge(t *testing.T) {
	m := New("t", "c")
	m.GCStarted()
	expected := `
# HELP dittofs_gc_running Number of block-store GC passes currently in progress (0 when idle; >1 when passes overlap). >0 indicates a pass is running.
# TYPE dittofs_gc_running gauge
dittofs_gc_running 1
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(expected), "dittofs_gc_running"); err != nil {
		t.Fatalf("gc running gauge mismatch: %v", err)
	}
}

// TestInstruments_GCRunningConcurrent asserts the running gauge uses
// reference-counted Inc/Dec, so a finishing pass does not prematurely clear the
// gauge while another pass is still in flight (the RunBlockGC vs
// RunBlockGCForShare overlap case).
func TestInstruments_GCRunningConcurrent(t *testing.T) {
	m := New("t", "c")
	m.GCStarted() // pass A in flight
	m.GCStarted() // pass B in flight; gauge == 2
	// Pass B finishes first; gauge must stay >0 because pass A is still running.
	m.GCFinished("ok", 0, 0, time.Second)
	expected := `
# HELP dittofs_gc_running Number of block-store GC passes currently in progress (0 when idle; >1 when passes overlap). >0 indicates a pass is running.
# TYPE dittofs_gc_running gauge
dittofs_gc_running 1
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(expected), "dittofs_gc_running"); err != nil {
		t.Fatalf("gc running gauge should stay 1 while a pass is still in flight: %v", err)
	}
	m.GCFinished("ok", 0, 0, time.Second) // pass A finishes; gauge back to 0
	expected0 := `
# HELP dittofs_gc_running Number of block-store GC passes currently in progress (0 when idle; >1 when passes overlap). >0 indicates a pass is running.
# TYPE dittofs_gc_running gauge
dittofs_gc_running 0
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(expected0), "dittofs_gc_running"); err != nil {
		t.Fatalf("gc running gauge should return to 0 after all passes finish: %v", err)
	}
}

func TestInstruments_Snapshot(t *testing.T) {
	m := New("t", "c")
	m.RecordSnapshotOp("create", "ok", 3*time.Second)
	m.RecordSnapshotOp("create", "error", time.Second)
	m.RecordSnapshotOp("delete", "ok", time.Millisecond)
	m.RecordSnapshotOp("restore", "ok", 10*time.Second)

	expected := `
# HELP dittofs_snapshot_operations_total Snapshot operations, by op (create|delete|restore) and result (ok|error).
# TYPE dittofs_snapshot_operations_total counter
dittofs_snapshot_operations_total{op="create",result="error"} 1
dittofs_snapshot_operations_total{op="create",result="ok"} 1
dittofs_snapshot_operations_total{op="delete",result="ok"} 1
dittofs_snapshot_operations_total{op="restore",result="ok"} 1
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(expected),
		"dittofs_snapshot_operations_total"); err != nil {
		t.Fatalf("snapshot mismatch: %v", err)
	}
	if got := testutil.CollectAndCount(m.Registry(), "dittofs_snapshot_duration_seconds"); got == 0 {
		t.Fatal("expected snapshot_duration_seconds series")
	}
}

func TestInstruments_NilSafe(t *testing.T) {
	var m *Metrics
	// Must not panic on nil receiver.
	m.RecordRequest("nfs", "READ", "ok", time.Millisecond)
	m.RecordConnAccepted("nfs")
	m.RecordConnClosed("nfs")
	m.RecordAuth("nfs", "krb5", false)
	m.RecordBackpressure(time.Second)
	m.RecordEviction(1024)
	m.GCStarted()
	m.GCFinished("ok", 1, 2, time.Second)
	m.RecordSnapshotOp("create", "ok", time.Second)
}
