package fixture

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/marmos91/dittofs/cmd/dfsctl/commands"
	"github.com/marmos91/dittofs/test/harness/registry"
)

// smbConformance is the directory of the SMB suites' scripts and shares file.
var smbConformance = filepath.Join("..", "..", "smb-conformance")

// The shares file creates the shares bootstrap.sh creates, in its order and
// each with its settings: for every share, the request the fixture sends is
// the one dfsctl sends when given that share's flags from share_create_args
// and the flags common_share_flags adds, with {user} as the owner.
func TestSharesMatchBootstrap(t *testing.T) {
	script, err := os.ReadFile(filepath.Join(smbConformance, "bootstrap.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(script), "\r\n", "\n")
	names := strings.Fields(regexp.MustCompile(`(?m)^SHARE_NAMES="([^"]*)"$`).FindStringSubmatch(text)[1])
	args := map[string]string{}
	for _, m := range regexp.MustCompile(`(?m)^\s*(/\S+)\) echo "([^"]*)" ;;$`).FindAllStringSubmatch(text, -1) {
		args[m[1]] = m[2]
	}
	common := regexp.MustCompile(`(?m)^\s*echo "(--metadata [^"]*)"$`).FindStringSubmatch(text)[1]
	common = strings.ReplaceAll(common, "wpts-admin", User)

	shares, err := loadShares(filepath.Join(smbConformance, "shares.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range shares {
		got = append(got, s.Name)
	}
	if !slices.Equal(got, names) {
		t.Fatalf("shares.yaml has %q, bootstrap.sh %q", got, names)
	}
	f, c, _ := newFakeAPI(t)
	for _, s := range shares {
		if err := addShare(c, registry.Fixture{Shares: "shares.yaml"}, s); err != nil {
			t.Fatal(err)
		}
	}
	for i, name := range names {
		want := dfsctlShare(t, strings.Fields(args[name]+" "+common))
		// dfsctl names the server's default permission, which the fixture
		// leaves to the server.
		if want["default_permission"] != "none" {
			t.Errorf("%s: dfsctl sent default_permission %v", name, want["default_permission"])
		}
		delete(want, "default_permission")
		if !reflect.DeepEqual(f.shares[i], want) {
			t.Errorf("%s: the fixture sends %v\ndfsctl sends %v", name, f.shares[i], want)
		}
	}
}

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
