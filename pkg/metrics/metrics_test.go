package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// fakeProvider returns a fixed snapshot for deterministic assertions.
type fakeProvider struct{ snap Snapshot }

func (f fakeProvider) MetricsSnapshot(context.Context) Snapshot { return f.snap }

func newTestMetrics(t *testing.T, snap Snapshot) *Metrics {
	t.Helper()
	m := New("v-test", "abc123")
	m.RegisterProvider(fakeProvider{snap: snap})
	return m
}

func TestRuntimeCollector_EmitsExpectedSeries(t *testing.T) {
	snap := Snapshot{
		Shares: []ShareSnapshot{{
			Name:          "alpha",
			DiskUsedBytes: 1024,
			DiskMaxBytes:  4096,
			UnsyncedBytes: 512,
			FailedSyncs:   3,
			RemoteHealthy: true,
			HasRemote:     true,
			LogicalBytes:  2000,
			FileCount:     7,
			SnapshotsHeld: 2,
		}},
		Quotas: []QuotaSnapshot{{
			Scope: "user", Principal: "1000", Share: "alpha",
			UsedBytes: 50, LimitBytes: 100, UsedInodes: 5, LimitInodes: 10,
		}},
		Clients: ClientSnapshot{NFS: 4, SMB: 1},
	}
	m := newTestMetrics(t, snap)

	expected := `
# HELP dittofs_localstore_disk_used_bytes Local block-store disk bytes in use.
# TYPE dittofs_localstore_disk_used_bytes gauge
dittofs_localstore_disk_used_bytes{share="alpha"} 1024
# HELP dittofs_sync_pending_bytes On-disk bytes present locally but not yet mirrored to the remote (data at risk).
# TYPE dittofs_sync_pending_bytes gauge
dittofs_sync_pending_bytes{share="alpha"} 512
# HELP dittofs_remote_up Whether the remote backend is currently healthy (1) or not (0).
# TYPE dittofs_remote_up gauge
dittofs_remote_up{share="alpha"} 1
# HELP dittofs_quota_used_bytes Bytes used by a quota principal.
# TYPE dittofs_quota_used_bytes gauge
dittofs_quota_used_bytes{principal="1000",scope="user",share="alpha"} 50
# HELP dittofs_client_connections_active Active client connections per protocol.
# TYPE dittofs_client_connections_active gauge
dittofs_client_connections_active{protocol="nfs"} 4
dittofs_client_connections_active{protocol="smb"} 1
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(expected),
		"dittofs_localstore_disk_used_bytes",
		"dittofs_sync_pending_bytes",
		"dittofs_remote_up",
		"dittofs_quota_used_bytes",
		"dittofs_client_connections_active",
	); err != nil {
		t.Fatalf("unexpected metrics: %v", err)
	}
}

// Sync/remote series must be suppressed for local-only shares (no remote).
func TestRuntimeCollector_LocalOnlyShareSkipsSyncSeries(t *testing.T) {
	m := newTestMetrics(t, Snapshot{Shares: []ShareSnapshot{{
		Name: "local", DiskUsedBytes: 10, HasRemote: false, UnsyncedBytes: 99,
	}}})
	if got := testutil.CollectAndCount(m.Registry(), "dittofs_sync_pending_bytes"); got != 0 {
		t.Fatalf("expected no sync_pending_bytes series for local-only share, got %d", got)
	}
	if got := testutil.CollectAndCount(m.Registry(), "dittofs_localstore_disk_used_bytes"); got != 1 {
		t.Fatalf("expected disk_used_bytes for local-only share, got %d", got)
	}
}

func TestBuildInfoPresent(t *testing.T) {
	m := New("v1.2.3", "deadbeef")
	expected := `
# HELP dittofs_build_info Build information for the running DittoFS server (always 1).
# TYPE dittofs_build_info gauge
dittofs_build_info{commit="deadbeef",version="v1.2.3"} 1
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(expected), "dittofs_build_info"); err != nil {
		t.Fatalf("build_info mismatch: %v", err)
	}
}

func TestHandlerServesText(t *testing.T) {
	m := newTestMetrics(t, Snapshot{Clients: ClientSnapshot{NFS: 2}})
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestWithAuth(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := withAuth("sekret", base)

	// Missing token → 401.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-token: want 401, got %d", rec.Code)
	}

	// Correct token → 200.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer sekret")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("good-token: want 200, got %d", rec.Code)
	}
}
