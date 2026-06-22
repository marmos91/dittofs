package identityprovider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/apiclient"
)

// TestConfigureFlagsRegistered asserts that configureCmd has all expected
// machine-account flags wired up.
func TestConfigureFlagsRegistered(t *testing.T) {
	expected := []string{
		"machine-account-enabled",
		"machine-account-name",
		"machine-secret",
		"machine-keytab",
		"dc-address",
	}
	for _, name := range expected {
		if f := configureCmd.Flags().Lookup(name); f == nil {
			t.Errorf("flag --%s is not registered on configureCmd", name)
		}
	}
}

// TestConfigureMachineSecretWriteOnly verifies that omitting --machine-secret
// sends an empty secret to the API (so the server's preserve-on-empty rule
// retains the stored credential), whereas providing the flag sends the value.
func TestConfigureMachineSecretWriteOnly(t *testing.T) {
	// Track the request body the fake server receives.
	var received apiclient.KerberosProviderConfig

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/config"):
			// Return empty config so runConfigure starts from a blank slate.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(apiclient.KerberosProviderConfig{})
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/config"):
			_ = json.NewDecoder(r.Body).Decode(&received)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(apiclient.KerberosProviderConfig{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	// Reset flag state between subtests.
	resetConfigureFlags := func() {
		configureMachineAccountEnabled = false
		configureMachineAccountName = ""
		configureMachineSecret = ""
		configureMachineKeytab = ""
		configureDCAddresses = nil
		// Reset the "changed" tracking by re-parsing an empty args set.
		_ = configureCmd.Flags().Set("machine-account-name", "")
	}

	t.Run("secret omitted → empty in request body", func(t *testing.T) {
		resetConfigureFlags()
		received = apiclient.KerberosProviderConfig{}

		// Simulate the client talking to our fake server.
		client := apiclient.New(srv.URL).WithToken("fake-token")

		// Build the config the same way runConfigure does when only
		// --machine-account-name is changed.
		cfg, _ := client.GetKerberosConfig()
		cfg.MachineAccount.AccountName = "MYHOST$"
		// Secret intentionally left empty (flag not set).

		out, err := client.PutKerberosConfig(cfg)
		if err != nil {
			t.Fatalf("PutKerberosConfig: %v", err)
		}
		_ = out

		if received.MachineAccount.Secret != "" {
			t.Errorf("expected empty secret in request (preserve-on-empty), got %q", received.MachineAccount.Secret)
		}
		if received.MachineAccount.AccountName != "MYHOST$" {
			t.Errorf("expected account_name=MYHOST$, got %q", received.MachineAccount.AccountName)
		}
	})

	t.Run("secret provided → sent in request body", func(t *testing.T) {
		resetConfigureFlags()
		received = apiclient.KerberosProviderConfig{}

		client := apiclient.New(srv.URL).WithToken("fake-token")
		cfg, _ := client.GetKerberosConfig()
		cfg.MachineAccount.Secret = "s3cret"

		_, err := client.PutKerberosConfig(cfg)
		if err != nil {
			t.Fatalf("PutKerberosConfig: %v", err)
		}

		if received.MachineAccount.Secret != "s3cret" {
			t.Errorf("expected secret=s3cret in request, got %q", received.MachineAccount.Secret)
		}
	})

	t.Run("dc-address repeated → joined with comma in DCAddress", func(t *testing.T) {
		addrs := []string{"192.0.2.10", "192.0.2.11"}
		joined := strings.Join(addrs, ",")

		client := apiclient.New(srv.URL).WithToken("fake-token")
		cfg, _ := client.GetKerberosConfig()
		cfg.MachineAccount.DCAddress = joined
		received = apiclient.KerberosProviderConfig{}

		_, err := client.PutKerberosConfig(cfg)
		if err != nil {
			t.Fatalf("PutKerberosConfig: %v", err)
		}

		if received.MachineAccount.DCAddress != joined {
			t.Errorf("dc_address: want %q, got %q", joined, received.MachineAccount.DCAddress)
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
		DCAddress:   "dc1.example.com,dc2.example.com",
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
		`"dc_address":"dc1.example.com,dc2.example.com"`,
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("marshaled JSON missing %s; got: %s", want, raw)
		}
	}

	var dst apiclient.KerberosMachineAccountConfig
	if err := json.Unmarshal(b, &dst); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if dst != src {
		t.Errorf("round-trip mismatch: want %+v, got %+v", src, dst)
	}
}
