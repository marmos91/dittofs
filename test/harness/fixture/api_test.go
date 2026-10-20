package fixture

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
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
	lease    int                       // what reading the settings back reports, when not 0
	deleg    *bool                     // the same for delegations, when not nil
	unbound  bool                      // reading back reports no registration with the host's rpcbind
	stores   map[string]map[string]any // the config of each store, by its path
	shares   []map[string]any          // each share created, as sent
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
	case r.URL.Path == "/api/v1/adapters/smb/settings":
		call += fmt.Sprintf(" %v", body)
		_, _ = w.Write([]byte("{}"))
	case r.URL.Path == "/api/v1/shares" && r.Method == http.MethodPost:
		f.shares = append(f.shares, body)
		call += fmt.Sprintf(" %v owner=%v", body["name"], body["owner"])
		_, _ = w.Write([]byte("{}"))
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
		register := f.settings["portmapper_register_with_system"] == true && !f.unbound
		_ = json.NewEncoder(w).Encode(map[string]any{"lease_time": lease, "delegations_enabled": deleg,
			"portmapper_register_with_system": register, "udp_enabled": f.settings["udp_enabled"] == true})
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
	f := &fakeAPI{running: map[string]int{"nfs": 12049, "smb": 12445}, stores: map[string]map[string]any{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c := apiclient.New(srv.URL)
	c.SetToken("t")
	return f, c, srv.URL
}

// testServer is a fixture f with its API at url and its data in /data, serving
// NFS on nfsPort and SMB on smbPort when they aren't 0, with the default
// share.
func testServer(f registry.Fixture, url string, nfsPort, smbPort int) *Server {
	return &Server{f: f, api: url, admin: "secret", data: "/data", shares: []apiclient.CreateShareRequest{{Name: Share}}, nfsPort: nfsPort, smbPort: smbPort, out: io.Discard}
}

// The server's own adapters go off first; the stores, the users, then the
// share, its squash and the users' grants go in; the NFS settings are set
// before NFS is enabled on the fixture's port, and read back once it runs.
// The check then finds NFS served there and nothing else.
func TestConfigureOrder(t *testing.T) {
	f, c, url := newFakeAPI(t)
	no := false
	s := testServer(registry.Fixture{Profile: "badger", NFS: registry.NFS{Version: "export"}, Squash: "none", Users: []string{"65532", "1"}, Delegations: &no, Lease: "30s"}, url, 40001, 0)
	if err := s.configure(c, nil, "pw"); err != nil {
		t.Fatal(err)
	}
	if err := s.check(c, nil); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"PUT /api/v1/adapters/nfs off",
		"PUT /api/v1/adapters/smb off",
		"POST /api/v1/store/metadata badger",
		"POST /api/v1/store/block memory",
		"POST /api/v1/users dt-65532 uid=65532 gid=65532",
		"POST /api/v1/users dt-1 uid=1 gid=1",
		"POST /api/v1/users dt-user uid=65535 gid=65535",
		"POST /api/v1/users dt-user2 uid=65536 gid=65536",
		"POST /api/v1/shares /export owner=<nil>",
		"PATCH /api/v1/shares/export/adapters/nfs/config",
		"PUT /api/v1/shares/export/permissions/users/dt-65532 read-write",
		"PUT /api/v1/shares/export/permissions/users/dt-1 read-write",
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

// A shares file's shares are each created with their own settings, owned by
// {user}, who gets the admin grant on each; SMB encryption is set before SMB
// is enabled on the fixture's port, and the check then finds SMB served
// there and nothing else.
func TestConfigureSMB(t *testing.T) {
	f, c, url := newFakeAPI(t)
	s := testServer(registry.Fixture{Profile: "memory", SMB: registry.SMB{Enabled: true, Encryption: "preferred"}, Shares: "shares.yaml"}, url, 0, 40002)
	on := true
	s.shares = []apiclient.CreateShareRequest{{Name: "/a"}, {Name: "/b", AccessBasedEnumeration: &on}}
	if err := s.configure(c, nil, "pw"); err != nil {
		t.Fatal(err)
	}
	if err := s.check(c, nil); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"PUT /api/v1/adapters/nfs off",
		"PUT /api/v1/adapters/smb off",
		"POST /api/v1/store/metadata badger",
		"POST /api/v1/store/block memory",
		"POST /api/v1/users dt-user uid=65535 gid=65535",
		"POST /api/v1/users dt-user2 uid=65536 gid=65536",
		"POST /api/v1/shares /a owner=dt-user",
		"PUT /api/v1/shares/a/permissions/users/dt-user admin",
		"POST /api/v1/shares /b owner=dt-user",
		"PUT /api/v1/shares/b/permissions/users/dt-user admin",
		"PATCH /api/v1/adapters/smb/settings map[enable_encryption:true]",
		"PUT /api/v1/adapters/smb on",
		"GET /api/v1/adapters",
	}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
	if f.running["smb"] != 40002 || f.shares[1]["access_based_enumeration"] != true {
		t.Errorf("SMB on %d, shares %v", f.running["smb"], f.shares)
	}
}

// Reset removes the share it's given, a test's {share}, and creates it again
// as configure did, with its settings, owner and grant, and touches no other
// share. A share the fixture doesn't have isn't reset.
func TestReset(t *testing.T) {
	f, _, url := newFakeAPI(t)
	s := testServer(registry.Fixture{Profile: "memory", SMB: registry.SMB{Enabled: true}, Shares: "shares.yaml"}, url, 0, 40002)
	on := true
	s.shares = []apiclient.CreateShareRequest{{Name: "/a"}, {Name: "/b", EncryptData: true, StreamsDisabled: &on}}
	if err := s.Reset("b"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"POST /api/v1/auth/login",
		"DELETE /api/v1/shares/b",
		"POST /api/v1/shares /b owner=dt-user",
		"PUT /api/v1/shares/b/permissions/users/dt-user admin",
	}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
	if sent := f.shares[0]; sent["encrypt_data"] != true || sent["streams_disabled"] != true {
		t.Errorf("the share recreated as %v", sent)
	}
	if err := s.Reset("c"); err == nil || len(f.calls) != len(want) {
		t.Errorf("a share the fixture lacks: %v, calls %q", err, f.calls)
	}
}

// A fixture without NFS settings or an export sets none and enables no
// adapter; one whose lease or delegations don't read back as set fails, as
// does a lease that isn't whole seconds.
func TestConfigureSettings(t *testing.T) {
	f, c, url := newFakeAPI(t)
	s := testServer(registry.Fixture{Profile: "memory"}, url, 0, 0)
	if err := s.configure(c, nil, "pw"); err != nil {
		t.Fatal(err)
	}
	if err := s.check(c, nil); err != nil {
		t.Errorf("no adapter asked for: %v", err)
	}
	if slices.ContainsFunc(f.calls, func(c string) bool { return strings.Contains(c, "settings") || strings.HasSuffix(c, " on") }) {
		t.Errorf("calls %q", f.calls)
	}

	f, c, url = newFakeAPI(t)
	f.lease = 90
	err := testServer(registry.Fixture{Profile: "memory", NFS: registry.NFS{Version: "export"}, Lease: "30s"}, url, 40001, 0).configure(c, nil, "pw")
	if err == nil || !strings.Contains(err.Error(), "lease of 90s, not the 30s") {
		t.Errorf("a lease that reads back wrong: %v", err)
	}

	f, c, url = newFakeAPI(t)
	yes, no := true, false
	f.deleg = &yes
	err = testServer(registry.Fixture{Profile: "memory", NFS: registry.NFS{Version: "export"}, Delegations: &no}, url, 40001, 0).configure(c, nil, "pw")
	if err == nil || !strings.Contains(err.Error(), "delegations true, not the false") {
		t.Errorf("delegations that read back wrong: %v", err)
	}

	_, c, url = newFakeAPI(t)
	if err := testServer(registry.Fixture{Profile: "memory", Lease: "1.5s"}, url, 0, 0).configure(c, nil, "pw"); err == nil || !strings.Contains(err.Error(), "whole number of seconds") {
		t.Errorf("a lease of 1.5s: %v", err)
	}
}

// self is the user dt runs as, in its own group, with a read-write grant like
// any user's. rpcbind: host has the server register with the host's rpcbind
// and serve UDP, as the setting it reads back must say.
func TestConfigureSelfAndRPCBind(t *testing.T) {
	f, c, url := newFakeAPI(t)
	s := testServer(registry.Fixture{Profile: "memory", NFS: registry.NFS{Version: "export"}, Users: []string{"self"}, RPCBind: "host"}, url, 40001, 0)
	if err := s.configure(c, nil, "pw"); err != nil {
		t.Fatal(err)
	}
	// A Windows process has no uid (os.Getuid is -1), and users: self runs on Linux only.
	if runtime.GOOS != "windows" {
		self := fmt.Sprintf("dt-%d", os.Getuid())
		for _, call := range []string{
			fmt.Sprintf("POST /api/v1/users %s uid=%d gid=%d", self, os.Getuid(), os.Getgid()),
			"PUT /api/v1/shares/export/permissions/users/" + self + " read-write",
		} {
			if !slices.Contains(f.calls, call) {
				t.Errorf("no %q among the calls:\n%s", call, strings.Join(f.calls, "\n"))
			}
		}
	}
	if f.settings["portmapper_register_with_system"] != true || f.settings["udp_enabled"] != true {
		t.Errorf("settings sent: %v", f.settings)
	}

	f, c, url = newFakeAPI(t)
	f.unbound = true
	err := testServer(registry.Fixture{Profile: "memory", NFS: registry.NFS{Version: "export"}, RPCBind: "host"}, url, 40001, 0).configure(c, nil, "pw")
	if err == nil || !strings.Contains(err.Error(), "doesn't register with the host's rpcbind") {
		t.Errorf("a registration that reads back off: %v", err)
	}
}

// The host's rpcbind must list the server's lock manager and MOUNT over TCP
// on its NFS port: another lock manager's registration, or the server's own
// on another port, doesn't count.
func TestUnregistered(t *testing.T) {
	const table = `   program vers proto   port  service
    100000    4   tcp    111  portmapper
    100021    4   udp  40001  nlockmgr
    100021    4   tcp  33421  nlockmgr
    100005    3   tcp  40001  mountd
`
	if got, nlm := unregistered(table, 40001); !slices.Equal(got, []string{"lock manager"}) || nlm != "33421" {
		t.Errorf("another lock manager's registration: missing %q, NLM at %q", got, nlm)
	}
	if got, nlm := unregistered(strings.Replace(table, "tcp  33421", "tcp  40001", 1), 40001); len(got) != 0 || nlm != "" {
		t.Errorf("both registered: missing %q, NLM at %q", got, nlm)
	}
	if got, _ := unregistered(table, 40002); !slices.Equal(got, []string{"MOUNT", "lock manager"}) {
		t.Errorf("another port: missing %q", got)
	}
}

// The check fails when NFS or SMB isn't served where the fixture asked, or
// when an adapter it didn't ask for runs.
func TestCheck(t *testing.T) {
	f, c, url := newFakeAPI(t)
	if err := testServer(registry.Fixture{}, url, 40001, 0).check(c, nil); err == nil || !strings.Contains(err.Error(), "smb adapter runs") {
		t.Errorf("the server's own SMB adapter: %v", err)
	}
	delete(f.running, "smb")
	if err := testServer(registry.Fixture{}, url, 40001, 0).check(c, nil); err == nil || !strings.Contains(err.Error(), "NFS isn't served on port 40001") {
		t.Errorf("NFS on 12049: %v", err)
	}
	f.running["smb"] = 12445
	delete(f.running, "nfs")
	if err := testServer(registry.Fixture{}, url, 0, 40002).check(c, nil); err == nil || !strings.Contains(err.Error(), "SMB isn't served on port 40002") {
		t.Errorf("SMB on 12445: %v", err)
	}
}

// The badger-s3 profile keeps its metadata on disk and its blocks in the
// fixture's bucket, under its own prefix; the check then fails when the
// server's own check of that block store doesn't find it healthy.
func TestConfigureS3(t *testing.T) {
	f, c, url := newFakeAPI(t)
	b := &bucket{endpoint: "http://10.0.0.5:4566", prefix: "dt-0123456789abcdef/"}
	s := testServer(registry.Fixture{Profile: "badger-s3"}, url, 0, 0)
	if err := s.configure(c, b, "pw"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.calls, "POST /api/v1/store/block s3") {
		t.Errorf("calls %q", f.calls)
	}
	blocks, meta := f.stores["/api/v1/store/block"], f.stores["/api/v1/store/metadata"]
	if blocks["bucket"] != bucketName || blocks["prefix"] != b.prefix || blocks["endpoint"] != b.endpoint || meta["path"] != filepath.Join("/data", "metadata") {
		t.Errorf("block store %v, metadata store %v", blocks, meta)
	}
	if err := s.check(c, b); err != nil {
		t.Errorf("a healthy block store: %v", err)
	}
	f.sick = "S3 health check failed: NotFound"
	if err := s.check(c, b); err == nil || !strings.Contains(err.Error(), "can't reach the bucket dittofs-dt at http://10.0.0.5:4566: S3 health check failed: NotFound") {
		t.Errorf("an unhealthy block store: %v", err)
	}
}
