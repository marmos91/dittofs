package fixture

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/marmos91/dittofs/cmd/dfsctl/commands"
	"github.com/marmos91/dittofs/test/harness/registry"
)

// smbConformance is the directory of the SMB suites' shares file and KDC image.
var smbConformance = filepath.Join("..", "..", "smb-conformance")

// dfsctlShare runs dfsctl share create with flags in this process, against a
// server that keeps what it is sent, and returns the share as sent.
func dfsctlShare(t *testing.T, flags []string) map[string]any {
	t.Helper()
	var sent map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&sent)
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()
	// dfsctl prints the share it created on stdout.
	stdout := os.Stdout
	os.Stdout, _ = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	defer func() { _ = os.Stdout.Close(); os.Stdout = stdout }()
	root := commands.GetRootCmd()
	root.SetArgs(append([]string{"--server", srv.URL, "--token", "t", "--output", "json", "share", "create"}, flags...))
	if err := root.Execute(); err != nil {
		t.Fatalf("dfsctl share create %s: %v", strings.Join(flags, " "), err)
	}
	// Cobra keeps a flag's value from one run to the next.
	create, _, _ := root.Find([]string{"share", "create"})
	create.Flags().VisitAll(func(f *pflag.Flag) {
		if f.Changed {
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		}
	})
	return sent
}

// A shares file names each share and sets only the flags dt knows, each
// true or false.
func TestSharesFileRefuses(t *testing.T) {
	for _, tc := range []struct{ yaml, want string }{
		{"- { encrypt-data: true }", "share 1 has no name"},
		{"- { name: /a, read-only: true }", "/a: read-only isn't a flag"},
		{"- { name: /a, encrypt-data: yes please }", "/a: encrypt-data isn't a flag"},
		{"- { name: /a, default-permission: true }", "/a: default-permission isn't a flag"},
		{"[]", "holds no share"},
	} {
		path := filepath.Join(t.TempDir(), "shares.yaml")
		if err := os.WriteFile(path, []byte(tc.yaml), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := loadShares(path); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", tc.yaml, err, tc.want)
		}
	}
}

// The share cthon04 and nfstest mount is the one the posix setup creates for
// them, with suites.json's default permission, owned by {user} as a shares
// file's shares are: the request the fixture sends is dfsctl's for those flags.
func TestNFSMountShare(t *testing.T) {
	shares, err := loadShares(filepath.Join("..", "..", "nfs-mount", "shares.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	f, c, _ := newFakeAPI(t)
	for _, s := range shares {
		if err := addShare(c, registry.Fixture{Shares: "shares.yaml"}, s); err != nil {
			t.Fatal(err)
		}
	}
	want := dfsctlShare(t, []string{"--name", Share, "--metadata", "default", "--block-store", "default", "--default-permission", "read", "--owner", User})
	if len(f.shares) != 1 || !reflect.DeepEqual(f.shares[0], want) {
		t.Errorf("the fixture sends %v\ndfsctl sends %v", f.shares, want)
	}
}
