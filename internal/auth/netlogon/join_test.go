package netlogon

import (
	"context"
	"strings"
	"testing"
	"unicode/utf16"

	ldapv3 "github.com/go-ldap/ldap/v3"
)

// fakeLDAP is an in-memory ldapConn that records the operations the join
// performs so tests can assert on the computer object and password write.
type fakeLDAP struct {
	existing  bool // computer already present on Search
	adds      []*ldapv3.AddRequest
	modifies  []*ldapv3.ModifyRequest
	bindErr   error
	addErr    error
	modifyErr error
	closed    bool
}

func (f *fakeLDAP) Bind(string, string) error { return f.bindErr }
func (f *fakeLDAP) Search(*ldapv3.SearchRequest) (*ldapv3.SearchResult, error) {
	res := &ldapv3.SearchResult{}
	if f.existing {
		res.Entries = []*ldapv3.Entry{{DN: "CN=DITTOFS,CN=Computers,DC=dittofs,DC=ad"}}
	}
	return res, nil
}
func (f *fakeLDAP) Add(r *ldapv3.AddRequest) error {
	if f.addErr != nil {
		return f.addErr
	}
	f.adds = append(f.adds, r)
	return nil
}
func (f *fakeLDAP) Modify(r *ldapv3.ModifyRequest) error {
	if f.modifyErr != nil {
		return f.modifyErr
	}
	f.modifies = append(f.modifies, r)
	return nil
}
func (f *fakeLDAP) Close() error { f.closed = true; return nil }

func baseJoinConfig() *JoinConfig {
	return &JoinConfig{
		LDAPURL:      "ldaps://dc.dittofs.ad",
		BindDN:       "CN=Administrator,CN=Users,DC=dittofs,DC=ad",
		BindPassword: "Passw0rd!2024",
		BaseDN:       "DC=dittofs,DC=ad",
		MachineName:  "DITTOFS",
	}
}

func TestJoinDirectory_CreatesComputerAndSetsPassword(t *testing.T) {
	fake := &fakeLDAP{existing: false}
	dial := func(context.Context, *JoinConfig) (ldapConn, error) { return fake, nil }

	if err := joinDirectory(context.Background(), dial, baseJoinConfig(), "Sup3rSecret!"); err != nil {
		t.Fatalf("joinDirectory: %v", err)
	}

	if len(fake.adds) != 1 {
		t.Fatalf("expected 1 Add (create computer), got %d", len(fake.adds))
	}
	add := fake.adds[0]
	if add.DN != "CN=DITTOFS,CN=Computers,DC=dittofs,DC=ad" {
		t.Errorf("computer DN = %q", add.DN)
	}
	if !hasAttr(add, "sAMAccountName", "DITTOFS$") {
		t.Errorf("sAMAccountName not set to DITTOFS$: %+v", add.Attributes)
	}
	if !hasAttr(add, "userAccountControl", "4096") {
		t.Errorf("userAccountControl not WORKSTATION_TRUST_ACCOUNT (4096): %+v", add.Attributes)
	}

	// Two modifies: unicodePwd + userAccountControl reassertion.
	if len(fake.modifies) != 2 {
		t.Fatalf("expected 2 Modify (unicodePwd + uac), got %d", len(fake.modifies))
	}
	assertUnicodePwd(t, fake.modifies[0], "Sup3rSecret!")
	if !fake.closed {
		t.Error("expected connection to be closed")
	}
}

func TestJoinDirectory_IdempotentWhenComputerExists(t *testing.T) {
	fake := &fakeLDAP{existing: true}
	dial := func(context.Context, *JoinConfig) (ldapConn, error) { return fake, nil }

	if err := joinDirectory(context.Background(), dial, baseJoinConfig(), "NewPass1!"); err != nil {
		t.Fatalf("joinDirectory: %v", err)
	}
	if len(fake.adds) != 0 {
		t.Errorf("expected no Add when computer exists, got %d", len(fake.adds))
	}
	// Still resets the password (re-join after lost secret) + reasserts uac.
	if len(fake.modifies) != 2 {
		t.Fatalf("expected 2 Modify on existing computer, got %d", len(fake.modifies))
	}
	assertUnicodePwd(t, fake.modifies[0], "NewPass1!")
}

func TestJoinDirectory_UsesOUWhenSet(t *testing.T) {
	cfg := baseJoinConfig()
	cfg.OU = "OU=Servers,DC=dittofs,DC=ad"
	fake := &fakeLDAP{}
	dial := func(context.Context, *JoinConfig) (ldapConn, error) { return fake, nil }

	if err := joinDirectory(context.Background(), dial, cfg, "x"); err != nil {
		t.Fatalf("joinDirectory: %v", err)
	}
	if fake.adds[0].DN != "CN=DITTOFS,OU=Servers,DC=dittofs,DC=ad" {
		t.Errorf("OU placement wrong: %q", fake.adds[0].DN)
	}
}

func TestJoinConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*JoinConfig)
		wantErr bool
	}{
		{"valid ldaps", func(*JoinConfig) {}, false},
		{"plaintext without starttls", func(c *JoinConfig) { c.LDAPURL = "ldap://dc.dittofs.ad" }, true},
		{"plaintext with starttls", func(c *JoinConfig) { c.LDAPURL = "ldap://dc.dittofs.ad"; c.StartTLS = true }, false},
		{"bad scheme", func(c *JoinConfig) { c.LDAPURL = "http://dc" }, true},
		{"empty url", func(c *JoinConfig) { c.LDAPURL = "" }, true},
		{"empty bind dn", func(c *JoinConfig) { c.BindDN = "" }, true},
		{"empty bind pw", func(c *JoinConfig) { c.BindPassword = "" }, true},
		{"empty base dn", func(c *JoinConfig) { c.BaseDN = "" }, true},
		{"empty machine name", func(c *JoinConfig) { c.MachineName = "" }, true},
		{"machine name with dollar", func(c *JoinConfig) { c.MachineName = "DITTOFS$" }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseJoinConfig()
			tt.mutate(cfg)
			err := cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() err=%v, wantErr=%v", err, tt.wantErr)
			}
		})
	}
}

func TestEncodeADPassword(t *testing.T) {
	got := encodeADPassword("ab")
	// Expect UTF-16LE of "ab" (quotes wrapped): " a b " → 4 code units, 8 bytes.
	want := []rune{'"', 'a', 'b', '"'}
	codes := utf16.Encode(want)
	var sb strings.Builder
	for _, c := range codes {
		sb.WriteByte(byte(c))
		sb.WriteByte(byte(c >> 8))
	}
	if got != sb.String() {
		t.Errorf("encodeADPassword mismatch:\n got %x\nwant %x", got, sb.String())
	}
}

func hasAttr(add *ldapv3.AddRequest, name, value string) bool {
	for _, a := range add.Attributes {
		if a.Type == name {
			for _, v := range a.Vals {
				if v == value {
					return true
				}
			}
		}
	}
	return false
}

func assertUnicodePwd(t *testing.T, mod *ldapv3.ModifyRequest, password string) {
	t.Helper()
	for _, ch := range mod.Changes {
		if ch.Modification.Type == "unicodePwd" {
			if len(ch.Modification.Vals) != 1 {
				t.Fatalf("unicodePwd should have 1 value, got %d", len(ch.Modification.Vals))
			}
			if ch.Modification.Vals[0] != encodeADPassword(password) {
				t.Errorf("unicodePwd value is not the UTF-16LE quoted password")
			}
			return
		}
	}
	t.Fatalf("no unicodePwd modification found in %+v", mod.Changes)
}
