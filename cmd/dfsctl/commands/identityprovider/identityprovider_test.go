package identityprovider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/apiclient"
)

// newTestServer starts a fake API server that returns an empty KerberosProviderConfig
// on GET and captures the PUT body into *received. The caller owns closing it.
func newTestServer(t *testing.T, received *apiclient.KerberosProviderConfig) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/config"):
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(apiclient.KerberosProviderConfig{})
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/config"):
			_ = json.NewDecoder(r.Body).Decode(received)
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(apiclient.KerberosProviderConfig{})
		default:
			http.NotFound(w, r)
		}
	}))
}

// runConfigureCmd builds a fresh configureCmd with a fake client pointing at
// srv, sets the given args (without the "kerberos" positional arg — that is
// prepended automatically), and executes it. Returns the execution error.
func runConfigureCmd(t *testing.T, srv *httptest.Server, extraArgs ...string) error {
	t.Helper()
	clientFn := func() (*apiclient.Client, error) {
		return apiclient.New(srv.URL).WithToken("fake-token"), nil
	}
	cmd := newConfigureCmd(clientFn)
	// Silence usage output on test errors.
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs(append([]string{"kerberos"}, extraArgs...))
	return cmd.Execute()
}

// TestConfigureFlagsRegistered asserts that a freshly built configureCmd has
// all expected machine-account flags wired up.
func TestConfigureFlagsRegistered(t *testing.T) {
	cmd := newConfigureCmd(nil)
	expected := []string{
		"machine-account-enabled",
		"machine-account-name",
		"machine-secret",
		"machine-keytab",
		"dc-address",
	}
	for _, name := range expected {
		if f := cmd.Flags().Lookup(name); f == nil {
			t.Errorf("flag --%s is not registered on configureCmd", name)
		}
	}
}

// TestConfigureMachineSecretWriteOnly verifies that:
//
//	(a) --machine-secret provided → secret sent in the request body;
//	(b) --machine-secret absent   → secret is empty in the request body
//	    (so the server's preserve-on-empty rule retains the stored credential);
//	(c) --dc-address repeated     → DCAddresses list in the request body;
//	(d) --machine-account-enabled → enabled=true in the request body;
//	    omitting it              → enabled stays false / not forced to true.
func TestConfigureMachineSecretWriteOnly(t *testing.T) {
	t.Run("secret provided → sent in request body", func(t *testing.T) {
		var received apiclient.KerberosProviderConfig
		srv := newTestServer(t, &received)
		defer srv.Close()

		if err := runConfigureCmd(t, srv, "--machine-account-name", "MYHOST$", "--machine-secret", "s3cret"); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if received.MachineAccount.Secret != "s3cret" {
			t.Errorf("expected secret=s3cret in request, got %q", received.MachineAccount.Secret)
		}
		if received.MachineAccount.AccountName != "MYHOST$" {
			t.Errorf("expected account_name=MYHOST$, got %q", received.MachineAccount.AccountName)
		}
	})

	t.Run("secret omitted → empty in request body", func(t *testing.T) {
		var received apiclient.KerberosProviderConfig
		srv := newTestServer(t, &received)
		defer srv.Close()

		if err := runConfigureCmd(t, srv, "--machine-account-name", "MYHOST$"); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if received.MachineAccount.Secret != "" {
			t.Errorf("expected empty secret in request (preserve-on-empty), got %q", received.MachineAccount.Secret)
		}
		if received.MachineAccount.AccountName != "MYHOST$" {
			t.Errorf("expected account_name=MYHOST$, got %q", received.MachineAccount.AccountName)
		}
	})

	t.Run("dc-address repeated → list in DCAddresses", func(t *testing.T) {
		var received apiclient.KerberosProviderConfig
		srv := newTestServer(t, &received)
		defer srv.Close()

		if err := runConfigureCmd(t, srv, "--dc-address", "192.0.2.10", "--dc-address", "192.0.2.11"); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		want := []string{"192.0.2.10", "192.0.2.11"}
		if len(received.MachineAccount.DCAddresses) != len(want) {
			t.Fatalf("dc_address: want %v, got %v", want, received.MachineAccount.DCAddresses)
		}
		for i, a := range want {
			if received.MachineAccount.DCAddresses[i] != a {
				t.Errorf("dc_address[%d]: want %q, got %q", i, a, received.MachineAccount.DCAddresses[i])
			}
		}
	})

	t.Run("machine-account-enabled provided → enabled=true", func(t *testing.T) {
		var received apiclient.KerberosProviderConfig
		srv := newTestServer(t, &received)
		defer srv.Close()

		if err := runConfigureCmd(t, srv, "--machine-account-enabled"); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if !received.MachineAccount.Enabled {
			t.Errorf("expected machine_account.enabled=true when flag is set")
		}
	})

	t.Run("machine-account-enabled omitted → enabled stays false", func(t *testing.T) {
		var received apiclient.KerberosProviderConfig
		srv := newTestServer(t, &received)
		defer srv.Close()

		// Only set the name; do NOT pass --machine-account-enabled.
		if err := runConfigureCmd(t, srv, "--machine-account-name", "MYHOST$"); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		// The server's initial GET returns enabled=false; our code must not force
		// it to true when the flag was not passed.
		if received.MachineAccount.Enabled {
			t.Errorf("expected machine_account.enabled=false when flag is absent")
		}
	})
}

// TestConfigureMachineAccountDTOFields confirms that KerberosMachineAccountConfig
// round-trips through JSON with the expected field names.
func TestConfigureMachineAccountDTOFields(t *testing.T) {
	src := apiclient.KerberosMachineAccountConfig{
		Enabled:     true,
		AccountName: "HOST$",
		Secret:      "pw",
		KeytabPath:  "/etc/krb5.keytab",
		DCAddresses: []string{"dc1.example.com", "dc2.example.com"},
	}
	b, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	raw := string(b)
	for _, want := range []string{
		`"enabled":true`,
		`"account_name":"HOST$"`,
		`"secret":"pw"`,
		`"keytab_path":"/etc/krb5.keytab"`,
		`"dc_address":["dc1.example.com","dc2.example.com"]`,
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("marshaled JSON missing %s; got: %s", want, raw)
		}
	}

	var dst apiclient.KerberosMachineAccountConfig
	if err := json.Unmarshal(b, &dst); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(dst.DCAddresses) != len(src.DCAddresses) {
		t.Fatalf("round-trip DCAddresses length: want %d, got %d", len(src.DCAddresses), len(dst.DCAddresses))
	}
	for i, a := range src.DCAddresses {
		if dst.DCAddresses[i] != a {
			t.Errorf("round-trip DCAddresses[%d]: want %q, got %q", i, a, dst.DCAddresses[i])
		}
	}
	// Check non-slice fields round-trip exactly.
	if dst.Enabled != src.Enabled || dst.AccountName != src.AccountName ||
		dst.Secret != src.Secret || dst.KeytabPath != src.KeytabPath {
		t.Errorf("round-trip mismatch (non-slice fields): want %+v, got %+v", src, dst)
	}
}
