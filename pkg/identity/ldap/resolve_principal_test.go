package ldap

import (
	"context"
	"strings"
	"testing"

	ldapv3 "github.com/go-ldap/ldap/v3"
)

// TestResolvePrincipalSID_Group covers name→SID resolution for a share grant
// (#1528): the sAMAccountName is extracted from a DOMAIN\name form, searched as
// a group, and the objectSid + Unix id are returned.
func TestResolvePrincipalSID_Group(t *testing.T) {
	const groupDN = "CN=Cubbit,CN=Users,DC=dittofs,DC=ad"

	fc := &fakeConn{
		search: func(req *ldapv3.SearchRequest) ([]*ldapv3.Entry, error) {
			if strings.Contains(req.Filter, "objectClass=group") && strings.Contains(req.Filter, "sAMAccountName=Cubbit") {
				return []*ldapv3.Entry{entry(groupDN, map[string][]string{
					"sAMAccountName": {"Cubbit"},
					"gidNumber":      {"10010"},
				}, encodeSID(t, 1104))}, nil
			}
			return nil, nil
		},
	}

	t.Run("rfc2307 returns gidNumber as the unix id", func(t *testing.T) {
		p := newTestProvider(t, baseCfg(), fc) // baseCfg is rfc2307
		got, found, err := p.ResolvePrincipalSID(context.Background(), `CUBBIT\Cubbit`, true)
		if err != nil {
			t.Fatalf("ResolvePrincipalSID: %v", err)
		}
		if !found {
			t.Fatal("expected found=true")
		}
		if got.SID != "S-1-5-21-1-2-3-1104" {
			t.Errorf("SID = %q, want S-1-5-21-1-2-3-1104", got.SID)
		}
		if got.UnixID != 10010 {
			t.Errorf("UnixID = %d, want 10010 (gidNumber)", got.UnixID)
		}
		if got.DisplayName != "Cubbit" {
			t.Errorf("DisplayName = %q, want Cubbit", got.DisplayName)
		}
	})

	t.Run("rid mode returns the RID as the unix id", func(t *testing.T) {
		cfg := baseCfg()
		cfg.Idmap = IdmapRID
		p := newTestProvider(t, cfg, fc)
		got, found, err := p.ResolvePrincipalSID(context.Background(), "Cubbit", true)
		if err != nil {
			t.Fatalf("ResolvePrincipalSID: %v", err)
		}
		if !found {
			t.Fatal("expected found=true")
		}
		if got.UnixID != 1104 {
			t.Errorf("UnixID = %d, want 1104 (RID)", got.UnixID)
		}
	})

	t.Run("no match returns found=false", func(t *testing.T) {
		empty := &fakeConn{search: func(*ldapv3.SearchRequest) ([]*ldapv3.Entry, error) { return nil, nil }}
		p := newTestProvider(t, baseCfg(), empty)
		_, found, err := p.ResolvePrincipalSID(context.Background(), "nope@DITTOFS.AD", true)
		if err != nil {
			t.Fatalf("ResolvePrincipalSID: %v", err)
		}
		if found {
			t.Error("expected found=false for a non-existent principal")
		}
	})
}

func TestSAMAccountNameFromPrincipal(t *testing.T) {
	cases := map[string]string{
		`CUBBIT\Cubbit`:      "Cubbit",
		"alice@cubbit.local": "alice",
		"Cubbit":             "Cubbit",
		`CUBBIT\alice@x`:     "alice", // both qualifiers stripped
	}
	for in, want := range cases {
		if got := sAMAccountNameFromPrincipal(in); got != want {
			t.Errorf("sAMAccountNameFromPrincipal(%q) = %q, want %q", in, got, want)
		}
	}
}
