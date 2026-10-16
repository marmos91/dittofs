package fixture

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/marmos91/dittofs/test/harness/registry"
)

// fakeAPI answers the calls the fixture makes as the server would, and
// records each, with an adapter's state and the NFS settings dt sets.
type fakeAPI struct {
	mu       sync.Mutex
	calls    []string
	running  map[string]int // adapter type to the port it serves, when enabled
	settings map[string]any
	lease    int   // what reading the settings back reports, when not 0
	deleg    *bool // the same for delegations, when not nil
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
	case r.URL.Path == "/api/v1/adapters/nfs/settings" && r.Method == http.MethodPatch:
		f.settings = body
		_, _ = w.Write([]byte("{}"))
	case r.URL.Path == "/api/v1/adapters/nfs/settings":
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
	case r.URL.Path == "/api/v1/auth/login":
		_, _ = w.Write([]byte(`{"access_token": "t"}`))
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

func newFakeAPI(t *testing.T) (*fakeAPI, *apiclient.Client, string) {
	t.Helper()
	f := &fakeAPI{running: map[string]int{"nfs": 12049, "smb": 12445}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c := apiclient.New(srv.URL)
	c.SetToken("t")
	return f, c, srv.URL
}

// The server's own adapters go off first; the stores, the share, its squash
// and the users with their grants go in; the NFS settings are set before NFS
// is enabled on the fixture's port, and read back once it runs. The check
// then finds NFS served there and nothing else.
func TestConfigureOrder(t *testing.T) {
	f, c, url := newFakeAPI(t)
	no := false
	fx := registry.Fixture{Profile: "badger", NFS: registry.NFS{Version: "export"}, Squash: "none", Users: []int{65532, 1}, Delegations: &no, Lease: "30s"}
	if err := configure(c, fx, "/data", "pw", 40001); err != nil {
		t.Fatal(err)
	}
	if err := check(context.Background(), c, url, 40001); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"PUT /api/v1/adapters/nfs off",
		"PUT /api/v1/adapters/smb off",
		"POST /api/v1/store/metadata",
		"POST /api/v1/store/block",
		"POST /api/v1/shares",
		"PATCH /api/v1/shares/export/adapters/nfs/config",
		"POST /api/v1/users dt-65532 uid=65532 gid=65532",
		"PUT /api/v1/shares/export/permissions/users/dt-65532 read-write",
		"POST /api/v1/users dt-1 uid=1 gid=1",
		"PUT /api/v1/shares/export/permissions/users/dt-1 read-write",
		"POST /api/v1/users dt-user uid=65535 gid=65535",
		"PATCH /api/v1/adapters/nfs/settings",
		"PUT /api/v1/adapters/nfs on",
		"GET /api/v1/adapters/nfs/settings",
		"GET /health/ready",
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
// adapter; one whose lease or delegations don't read back as set fails, as
// does a lease that isn't whole seconds.
func TestConfigureSettings(t *testing.T) {
	f, c, url := newFakeAPI(t)
	if err := configure(c, registry.Fixture{Profile: "memory"}, "/data", "pw", 0); err != nil {
		t.Fatal(err)
	}
	if err := check(context.Background(), c, url, 0); err != nil {
		t.Errorf("no adapter asked for: %v", err)
	}
	if slices.ContainsFunc(f.calls, func(c string) bool { return strings.Contains(c, "settings") || strings.HasSuffix(c, " on") }) {
		t.Errorf("calls %q", f.calls)
	}

	f, c, _ = newFakeAPI(t)
	f.lease = 90
	err := configure(c, registry.Fixture{Profile: "memory", NFS: registry.NFS{Version: "export"}, Lease: "30s"}, "/data", "pw", 40001)
	if err == nil || !strings.Contains(err.Error(), "lease of 90s, not the 30s") {
		t.Errorf("a lease that reads back wrong: %v", err)
	}

	f, c, _ = newFakeAPI(t)
	yes, no := true, false
	f.deleg = &yes
	err = configure(c, registry.Fixture{Profile: "memory", NFS: registry.NFS{Version: "export"}, Delegations: &no}, "/data", "pw", 40001)
	if err == nil || !strings.Contains(err.Error(), "delegations true, not the false") {
		t.Errorf("delegations that read back wrong: %v", err)
	}

	_, c, _ = newFakeAPI(t)
	if err := configure(c, registry.Fixture{Profile: "memory", Lease: "1.5s"}, "/data", "pw", 0); err == nil || !strings.Contains(err.Error(), "whole number of seconds") {
		t.Errorf("a lease of 1.5s: %v", err)
	}
}

// The check fails when NFS isn't served where the fixture asked, or when an
// adapter it didn't ask for runs.
func TestCheck(t *testing.T) {
	f, c, url := newFakeAPI(t)
	if err := check(context.Background(), c, url, 40001); err == nil || !strings.Contains(err.Error(), "smb adapter runs") {
		t.Errorf("the server's own SMB adapter: %v", err)
	}
	delete(f.running, "smb")
	if err := check(context.Background(), c, url, 40001); err == nil || !strings.Contains(err.Error(), "NFS isn't served on port 40001") {
		t.Errorf("NFS on 12049: %v", err)
	}
}
