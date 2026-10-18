package fixture

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/marmos91/dittofs/test/harness/registry"
)

// fakeAPI answers the calls the fixture makes as the server would, and
// records each, with an adapter's state, the NFS settings dt sets and the
// stores it creates.
type fakeAPI struct {
	mu       sync.Mutex
	calls    []string
	running  map[string]int // adapter type to the port it serves, when enabled
	settings map[string]any
	lease    int                       // the lease the answer to the settings reports, when not 0
	deleg    *bool                     // the same for delegations, when not nil
	stores   map[string]map[string]any // the config of each store, by its path
	sick     string                    // when set, the block store's health check fails with it
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := r.Method + " " + r.URL.Path
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	adapter, isAdapter := strings.CutPrefix(r.URL.Path, "/api/v1/adapters/")
	switch {
	case r.Method == http.MethodPut && isAdapter:
		if body["enabled"] == true {
			f.running[adapter] = int(body["port"].(float64))
			call += " on"
		} else {
			delete(f.running, adapter)
			call += " off"
		}
		_ = json.NewEncoder(w).Encode(apiclient.Adapter{Type: adapter, Port: f.running[adapter], Enabled: f.running[adapter] > 0, Running: f.running[adapter] > 0})
	case r.URL.Path == "/api/v1/adapters/nfs/settings":
		f.settings = body
		lease, _ := f.settings["lease_time"].(float64)
		if f.lease != 0 {
			lease = float64(f.lease)
		}
		deleg, _ := f.settings["delegations_enabled"].(bool)
		if f.deleg != nil {
			deleg = *f.deleg
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"lease_time": lease, "delegations_enabled": deleg})
	case r.URL.Path == "/api/v1/adapters":
		var list []apiclient.Adapter
		for _, a := range []string{"nfs", "smb"} {
			list = append(list, apiclient.Adapter{Type: a, Port: f.running[a], Enabled: f.running[a] > 0, Running: f.running[a] > 0})
		}
		_ = json.NewEncoder(w).Encode(list)
	case strings.HasPrefix(r.URL.Path, "/api/v1/store/") && r.Method == http.MethodPost:
		var config map[string]any
		_ = json.Unmarshal([]byte(fmt.Sprint(body["config"])), &config)
		f.stores[r.URL.Path] = config
		call += fmt.Sprintf(" %v", body["type"])
		_, _ = w.Write([]byte("{}"))
	case r.URL.Path == "/api/v1/store/block/default/health":
		_ = json.NewEncoder(w).Encode(apiclient.BlockStoreHealthResult{Healthy: f.sick == "", Details: f.sick})
	case strings.HasPrefix(r.URL.Path, "/api/v1/users"):
		call += fmt.Sprintf(" %v uid=%v gid=%v", body["username"], body["uid"], body["gid"])
		_, _ = w.Write([]byte("{}"))
	case strings.Contains(r.URL.Path, "/permissions/users/"):
		call += fmt.Sprintf(" %v", body["level"])
	default:
		_, _ = w.Write([]byte("{}"))
	}
	f.calls = append(f.calls, call)
}

func newFakeAPI(t *testing.T) (*fakeAPI, *apiclient.Client) {
	t.Helper()
	f := &fakeAPI{running: map[string]int{"nfs": 12049, "smb": 12445}, stores: map[string]map[string]any{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c := apiclient.New(srv.URL)
	c.SetToken("t")
	return f, c
}

// The server's own adapters go off first; the stores, the share, its squash
// and the users with their grants go in; the NFS settings are set before NFS
// is enabled on the fixture's port. The check then finds NFS served there and
// nothing else.
func TestConfigureOrder(t *testing.T) {
	f, c := newFakeAPI(t)
	no := false
	fx := registry.Fixture{Profile: "badger", NFS: registry.NFS{Version: "export"}, Squash: "none", Users: []int{65532, 1}, Delegations: &no, Lease: "30s"}
	if err := configure(c, fx, nil, "/data", "pw", 40001); err != nil {
		t.Fatal(err)
	}
	if err := check(c, nil, 40001); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"PUT /api/v1/adapters/nfs off",
		"PUT /api/v1/adapters/smb off",
		"POST /api/v1/store/metadata badger",
		"POST /api/v1/store/block memory",
		"POST /api/v1/shares",
		"PATCH /api/v1/shares/export/adapters/nfs/config",
		"POST /api/v1/users dt-65532 uid=65532 gid=65532",
		"PUT /api/v1/shares/export/permissions/users/dt-65532 read-write",
		"POST /api/v1/users dt-1 uid=1 gid=1",
		"PUT /api/v1/shares/export/permissions/users/dt-1 read-write",
		"POST /api/v1/users dt-user uid=65535 gid=65535",
		"PATCH /api/v1/adapters/nfs/settings",
		"PUT /api/v1/adapters/nfs on",
		"GET /api/v1/adapters",
	}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
	if f.settings["lease_time"] != float64(30) || f.settings["delegations_enabled"] != false || len(f.settings) != 2 {
		t.Errorf("settings sent: %v", f.settings)
	}
}

// A fixture without NFS settings or an export sets none and enables no
// adapter; one whose lease or delegations the server's answer doesn't hold
// as set fails, as does a lease that isn't whole seconds.
func TestConfigureSettings(t *testing.T) {
	f, c := newFakeAPI(t)
	if err := configure(c, registry.Fixture{Profile: "memory"}, nil, "/data", "pw", 0); err != nil {
		t.Fatal(err)
	}
	if err := check(c, nil, 0); err != nil {
		t.Errorf("no adapter asked for: %v", err)
	}
	if slices.ContainsFunc(f.calls, func(c string) bool { return strings.Contains(c, "settings") || strings.HasSuffix(c, " on") }) {
		t.Errorf("calls %q", f.calls)
	}

	f, c = newFakeAPI(t)
	f.lease = 90
	err := configure(c, registry.Fixture{Profile: "memory", NFS: registry.NFS{Version: "export"}, Lease: "30s"}, nil, "/data", "pw", 40001)
	if err == nil || !strings.Contains(err.Error(), "lease of 90s, not the 30s") {
		t.Errorf("a lease the server answers wrong: %v", err)
	}

	f, c = newFakeAPI(t)
	yes, no := true, false
	f.deleg = &yes
	err = configure(c, registry.Fixture{Profile: "memory", NFS: registry.NFS{Version: "export"}, Delegations: &no}, nil, "/data", "pw", 40001)
	if err == nil || !strings.Contains(err.Error(), "delegations true, not the false") {
		t.Errorf("delegations the server answers wrong: %v", err)
	}

	_, c = newFakeAPI(t)
	if err := configure(c, registry.Fixture{Profile: "memory", Lease: "1.5s"}, nil, "/data", "pw", 0); err == nil || !strings.Contains(err.Error(), "whole number of seconds") {
		t.Errorf("a lease of 1.5s: %v", err)
	}
}

// The check fails when NFS isn't served where the fixture asked, or when an
// adapter it didn't ask for runs.
func TestCheck(t *testing.T) {
	f, c := newFakeAPI(t)
	if err := check(c, nil, 40001); err == nil || !strings.Contains(err.Error(), "smb adapter runs") {
		t.Errorf("the server's own SMB adapter: %v", err)
	}
	delete(f.running, "smb")
	if err := check(c, nil, 40001); err == nil || !strings.Contains(err.Error(), "NFS isn't served on port 40001") {
		t.Errorf("NFS on 12049: %v", err)
	}
}

// The badger-s3 profile keeps its metadata on disk and its blocks in the
// fixture's bucket, under its own prefix; the check then fails when the
// server's own check of that block store doesn't find it healthy.
func TestConfigureS3(t *testing.T) {
	f, c := newFakeAPI(t)
	b := &bucket{endpoint: "http://10.0.0.5:4566", prefix: "dt-0123456789abcdef/"}
	if err := configure(c, registry.Fixture{Profile: "badger-s3"}, b, "/data", "pw", 0); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.calls, "POST /api/v1/store/block s3") {
		t.Errorf("calls %q", f.calls)
	}
	blocks, meta := f.stores["/api/v1/store/block"], f.stores["/api/v1/store/metadata"]
	if blocks["bucket"] != bucketName || blocks["prefix"] != b.prefix || blocks["endpoint"] != b.endpoint || meta["path"] != filepath.Join("/data", "metadata") {
		t.Errorf("block store %v, metadata store %v", blocks, meta)
	}
	if err := check(c, b, 0); err != nil {
		t.Errorf("a healthy block store: %v", err)
	}
	f.sick = "S3 health check failed: NotFound"
	if err := check(c, b, 0); err == nil || !strings.Contains(err.Error(), "can't reach the bucket dittofs-dt at http://10.0.0.5:4566: S3 health check failed: NotFound") {
		t.Errorf("an unhealthy block store: %v", err)
	}
}
