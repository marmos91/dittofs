package share

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/pflag"
)

// Refs #514 — dfsctl flag plumbing for --acl-canonicalize-inherited.

// shareJSONBodyServer records the JSON body sent for POST/PUT requests and
// answers with a single Share — used to assert dfsctl wire payloads.
type shareJSONBodyServer struct {
	*httptest.Server
	lastBody []byte
	lastPath string
	lastVerb string
}

func newShareJSONBodyServer(t *testing.T) *shareJSONBodyServer {
	t.Helper()
	s := &shareJSONBodyServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.lastVerb = r.Method
		s.lastPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		s.lastBody = b
		w.Header().Set("Content-Type", "application/json")
		// Decode a CreateShareRequest or UpdateShareRequest to echo the
		// AclFlag back; default to false.
		acl := true
		// Try Create first.
		var cr apiclient.CreateShareRequest
		if err := json.Unmarshal(b, &cr); err == nil && cr.AclFlagInheritedCanonicalization != nil {
			acl = *cr.AclFlagInheritedCanonicalization
		} else {
			var ur apiclient.UpdateShareRequest
			if err := json.Unmarshal(b, &ur); err == nil && ur.AclFlagInheritedCanonicalization != nil {
				acl = *ur.AclFlagInheritedCanonicalization
			}
		}
		status := http.StatusOK
		if r.Method == http.MethodPost {
			status = http.StatusCreated
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(apiclient.Share{
			ID:                               "id1",
			Name:                             "/x",
			AclFlagInheritedCanonicalization: acl,
		})
	}))
	return s
}

// resetCreateFlags clears package-level state + Cobra's `Changed`
// bookkeeping so each test starts from a known baseline.
func resetCreateFlags() {
	createName = ""
	createMetadata = ""
	createLocal = ""
	createRemote = ""
	createReadOnly = false
	createEncryptData = false
	createDefaultPermission = "read-write"
	createDescription = ""
	createRetention = ""
	createRetentionTTL = ""
	createLocalStoreSize = ""
	createReadBufferSize = ""
	createQuotaBytes = ""
	createAclCanonicalize = true
	createCmd.Flags().VisitAll(func(f *pflag.Flag) { f.Changed = false })
}

// resetEditFlags clears editCmd state for tests.
func resetEditFlags() {
	editLocal = ""
	editRemote = ""
	editReadOnly = ""
	editEncryptData = ""
	editDefaultPermission = ""
	editDescription = ""
	editRetention = ""
	editRetentionTTL = ""
	editLocalStoreSize = ""
	editReadBufferSize = ""
	editQuotaBytes = ""
	editAclCanonicalize = ""
	editCmd.Flags().VisitAll(func(f *pflag.Flag) { f.Changed = false })
}

// TestCreateCmd_AclCanonicalizeInherited_ExplicitFalse drives `share create`
// with the new flag set to false and asserts the wire payload carries
// "acl_flag_inherited_canonicalization":false.
func TestCreateCmd_AclCanonicalizeInherited_ExplicitFalse(t *testing.T) {
	s := newShareJSONBodyServer(t)
	defer s.Close()
	withTestServer(t, s.URL)

	resetCreateFlags()
	createName = "/x"
	createMetadata = "meta"
	createLocal = "bs"
	createAclCanonicalize = false
	if err := createCmd.Flags().Set("acl-canonicalize-inherited", "false"); err != nil {
		t.Fatalf("Flags.Set: %v", err)
	}

	_ = captureStdout(t, func() {
		if err := runCreate(createCmd, nil); err != nil {
			t.Fatalf("runCreate: %v", err)
		}
	})

	if s.lastVerb != http.MethodPost {
		t.Fatalf("verb = %q, want POST", s.lastVerb)
	}
	if s.lastPath != "/api/v1/shares" {
		t.Fatalf("path = %q, want /api/v1/shares", s.lastPath)
	}
	if !strings.Contains(string(s.lastBody), `"acl_flag_inherited_canonicalization":false`) {
		t.Errorf("wire body missing acl_flag_inherited_canonicalization:false; got %s", s.lastBody)
	}
}

// TestCreateCmd_AclCanonicalizeInherited_UnsetOmitsField confirms the flag
// is omitted from the wire payload when callers don't pass it — the server
// applies its own default (true).
func TestCreateCmd_AclCanonicalizeInherited_UnsetOmitsField(t *testing.T) {
	s := newShareJSONBodyServer(t)
	defer s.Close()
	withTestServer(t, s.URL)

	resetCreateFlags()
	createName = "/x"
	createMetadata = "meta"
	createLocal = "bs"

	_ = captureStdout(t, func() {
		if err := runCreate(createCmd, nil); err != nil {
			t.Fatalf("runCreate: %v", err)
		}
	})

	if strings.Contains(string(s.lastBody), "acl_flag_inherited_canonicalization") {
		t.Errorf("wire body must omit acl_flag_inherited_canonicalization when flag unset; got %s", s.lastBody)
	}
}

// TestEditCmd_AclCanonicalizeInherited_ExplicitFalse drives `share edit`
// with --acl-canonicalize-inherited=false and asserts the wire payload.
func TestEditCmd_AclCanonicalizeInherited_ExplicitFalse(t *testing.T) {
	s := newShareJSONBodyServer(t)
	defer s.Close()
	withTestServer(t, s.URL)

	resetEditFlags()
	editAclCanonicalize = "false"
	if err := editCmd.Flags().Set("acl-canonicalize-inherited", "false"); err != nil {
		t.Fatalf("Flags.Set: %v", err)
	}

	_ = captureStdout(t, func() {
		if err := runEdit(editCmd, []string{"x"}); err != nil {
			t.Fatalf("runEdit: %v", err)
		}
	})

	if s.lastVerb != http.MethodPut {
		t.Fatalf("verb = %q, want PUT", s.lastVerb)
	}
	if !strings.Contains(string(s.lastBody), `"acl_flag_inherited_canonicalization":false`) {
		t.Errorf("edit wire body missing acl_flag_inherited_canonicalization:false; got %s", s.lastBody)
	}
}

// TestEditCmd_AclCanonicalizeInherited_StrictParse verifies that the
// edit handler rejects any value other than the literal "true"/"false"
// (case-insensitive). Refs #514: a permissive `ToLower == "true"`
// silently coerces typos and bool-ish synonyms (yes/1) to false, which
// is undetectable from the operator's side. The strict parse turns
// those into a hard error and accepts only the canonical forms.
func TestEditCmd_AclCanonicalizeInherited_StrictParse(t *testing.T) {
	tests := []struct {
		name      string
		flagValue string
		wantErr   bool
		// wantBoolInBody only checked when wantErr is false.
		wantBoolInBody string
	}{
		{name: "yes is rejected", flagValue: "yes", wantErr: true},
		{name: "1 is rejected", flagValue: "1", wantErr: true},
		{name: "typo truee is rejected", flagValue: "truee", wantErr: true},
		{name: "empty after trim is rejected", flagValue: "   ", wantErr: true},
		{name: "canonical true accepted", flagValue: "true", wantErr: false, wantBoolInBody: `"acl_flag_inherited_canonicalization":true`},
		{name: "canonical false accepted", flagValue: "false", wantErr: false, wantBoolInBody: `"acl_flag_inherited_canonicalization":false`},
		{name: "uppercase FALSE accepted", flagValue: "FALSE", wantErr: false, wantBoolInBody: `"acl_flag_inherited_canonicalization":false`},
		{name: "mixed-case True accepted", flagValue: "True", wantErr: false, wantBoolInBody: `"acl_flag_inherited_canonicalization":true`},
		{name: "padded true accepted", flagValue: "  true  ", wantErr: false, wantBoolInBody: `"acl_flag_inherited_canonicalization":true`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newShareJSONBodyServer(t)
			defer s.Close()
			withTestServer(t, s.URL)

			resetEditFlags()
			editAclCanonicalize = tc.flagValue
			if err := editCmd.Flags().Set("acl-canonicalize-inherited", tc.flagValue); err != nil {
				t.Fatalf("Flags.Set: %v", err)
			}

			var runErr error
			_ = captureStdout(t, func() {
				runErr = runEdit(editCmd, []string{"x"})
			})

			if tc.wantErr {
				if runErr == nil {
					t.Fatalf("expected error for flag value %q, got nil; body=%s", tc.flagValue, s.lastBody)
				}
				if !strings.Contains(runErr.Error(), "--acl-canonicalize-inherited") {
					t.Errorf("error %q should reference --acl-canonicalize-inherited", runErr.Error())
				}
				if s.lastVerb != "" {
					t.Errorf("expected no HTTP request to fire on parse error; got %s %s", s.lastVerb, s.lastPath)
				}
				return
			}

			if runErr != nil {
				t.Fatalf("unexpected error for flag value %q: %v", tc.flagValue, runErr)
			}
			if s.lastVerb != http.MethodPut {
				t.Fatalf("verb = %q, want PUT", s.lastVerb)
			}
			if !strings.Contains(string(s.lastBody), tc.wantBoolInBody) {
				t.Errorf("wire body missing %s; got %s", tc.wantBoolInBody, s.lastBody)
			}
		})
	}
}
