package ldap

import (
	"bytes"
	"context"
	"strings"
	"testing"

	ldapv3 "github.com/go-ldap/ldap/v3"

	"github.com/marmos91/dittofs/pkg/auth/sid"
	"github.com/marmos91/dittofs/pkg/identity"
)

// fakeConn is an in-memory directory used to exercise Provider.Resolve without
// a network. It answers searches from a small fixed object set.
type fakeConn struct {
	bindErr error
	// search is invoked for each search request; returns the matching entries.
	search func(req *ldapv3.SearchRequest) ([]*ldapv3.Entry, error)
}

func (f *fakeConn) Bind(string, string) error { return f.bindErr }
func (f *fakeConn) Search(req *ldapv3.SearchRequest) (*ldapv3.SearchResult, error) {
	entries, err := f.search(req)
	if err != nil {
		return nil, err
	}
	return &ldapv3.SearchResult{Entries: entries}, nil
}
func (f *fakeConn) Close() error { return nil }

// encodeSID returns the binary objectSid for "S-1-5-21-1-2-3-<rid>".
func encodeSID(t *testing.T, rid uint32) []byte {
	t.Helper()
	s := &sid.SID{
		Revision:            1,
		SubAuthorityCount:   5,
		IdentifierAuthority: [6]byte{0, 0, 0, 0, 0, 5},
		SubAuthorities:      []uint32{21, 1, 2, 3, rid},
	}
	var buf bytes.Buffer
	sid.EncodeSID(&buf, s)
	return buf.Bytes()
}

func entry(dn string, attrs map[string][]string, sidBytes []byte) *ldapv3.Entry {
	e := ldapv3.NewEntry(dn, attrs)
	if sidBytes != nil {
		e.Attributes = append(e.Attributes, &ldapv3.EntryAttribute{
			Name:       "objectSid",
			ByteValues: [][]byte{sidBytes},
		})
	}
	return e
}

func newTestProvider(t *testing.T, cfg *Config, fc *fakeConn) *Provider {
	t.Helper()
	p, err := New(cfg, nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p.connect = func(context.Context, *Config) (conn, error) { return fc, nil }
	return p
}

func baseCfg() *Config {
	return &Config{
		Enabled:      true,
		URL:          "ldaps://dc.dittofs.ad:636",
		BaseDN:       "DC=dittofs,DC=ad",
		BindDN:       "CN=svc,DC=dittofs,DC=ad",
		BindPassword: "pw",
		Realm:        "DITTOFS.AD",
		Idmap:        IdmapRFC2307,
		NestedGroups: true,
	}
}

func TestResolve_RFC2307_WithNestedGroups(t *testing.T) {
	aliceDN := "CN=alice,CN=Users,DC=dittofs,DC=ad"
	devsDN := "CN=devs,CN=Users,DC=dittofs,DC=ad"
	engDN := "CN=engineering,CN=Users,DC=dittofs,DC=ad"

	fc := &fakeConn{
		search: func(req *ldapv3.SearchRequest) ([]*ldapv3.Entry, error) {
			switch {
			case strings.Contains(req.Filter, "sAMAccountName=alice"):
				return []*ldapv3.Entry{entry(aliceDN, map[string][]string{
					"sAMAccountName": {"alice"},
					"uidNumber":      {"10001"},
					"gidNumber":      {"10000"},
				}, encodeSID(t, 1104))}, nil
			case strings.Contains(req.Filter, matchingRuleInChain):
				// Nested group resolution returns BOTH devs and engineering.
				return []*ldapv3.Entry{
					entry(devsDN, nil, nil),
					entry(engDN, nil, nil),
				}, nil
			case req.BaseDN == devsDN:
				return []*ldapv3.Entry{entry(devsDN, map[string][]string{"gidNumber": {"10010"}}, encodeSID(t, 1105))}, nil
			case req.BaseDN == engDN:
				return []*ldapv3.Entry{entry(engDN, map[string][]string{"gidNumber": {"10011"}}, encodeSID(t, 1106))}, nil
			}
			return nil, nil
		},
	}

	p := newTestProvider(t, baseCfg(), fc)
	res, err := p.Resolve(context.Background(), &identity.Credential{ExternalID: "alice@DITTOFS.AD"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !res.Found {
		t.Fatal("expected Found=true")
	}
	if res.UID != 10001 || res.GID != 10000 {
		t.Errorf("UID/GID = %d/%d, want 10001/10000", res.UID, res.GID)
	}
	if res.Domain != "DITTOFS.AD" {
		t.Errorf("Domain = %q", res.Domain)
	}
	wantGIDs := map[uint32]bool{10000: false, 10010: false, 10011: false}
	for _, g := range res.GIDs {
		if _, ok := wantGIDs[g]; ok {
			wantGIDs[g] = true
		}
	}
	for g, seen := range wantGIDs {
		if !seen {
			t.Errorf("missing expected GID %d in %v", g, res.GIDs)
		}
	}
}

func TestResolve_RIDFallback_WhenNoRFC2307(t *testing.T) {
	cfg := baseCfg()
	cfg.Idmap = IdmapRID
	cfg.NestedGroups = false

	bobDN := "CN=bob,CN=Users,DC=dittofs,DC=ad"
	engDN := "CN=engineering,CN=Users,DC=dittofs,DC=ad"

	fc := &fakeConn{
		search: func(req *ldapv3.SearchRequest) ([]*ldapv3.Entry, error) {
			switch {
			case strings.Contains(req.Filter, "sAMAccountName=bob"):
				return []*ldapv3.Entry{entry(bobDN, map[string][]string{
					"sAMAccountName": {"bob"},
					"memberOf":       {engDN},
				}, encodeSID(t, 1200))}, nil
			case req.BaseDN == engDN:
				return []*ldapv3.Entry{entry(engDN, nil, encodeSID(t, 1300))}, nil
			}
			return nil, nil
		},
	}

	p := newTestProvider(t, cfg, fc)
	res, err := p.Resolve(context.Background(), &identity.Credential{ExternalID: "bob@DITTOFS.AD"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !res.Found {
		t.Fatal("expected Found=true")
	}
	// RID fallback: UID == GID == RID (1200).
	if res.UID != 1200 || res.GID != 1200 {
		t.Errorf("UID/GID = %d/%d, want 1200/1200 (RID)", res.UID, res.GID)
	}
	// engineering group GID derives from its RID (1300).
	found := false
	for _, g := range res.GIDs {
		if g == 1300 {
			found = true
		}
	}
	if !found {
		t.Errorf("expected engineering RID 1300 in GIDs %v", res.GIDs)
	}
}

func TestResolve_NotFound(t *testing.T) {
	fc := &fakeConn{search: func(*ldapv3.SearchRequest) ([]*ldapv3.Entry, error) { return nil, nil }}
	p := newTestProvider(t, baseCfg(), fc)
	res, err := p.Resolve(context.Background(), &identity.Credential{ExternalID: "ghost@DITTOFS.AD"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Found {
		t.Error("expected Found=false for missing user")
	}
}

func TestResolve_BySID(t *testing.T) {
	dn := "CN=alice,CN=Users,DC=dittofs,DC=ad"
	fc := &fakeConn{
		search: func(req *ldapv3.SearchRequest) ([]*ldapv3.Entry, error) {
			if strings.Contains(req.Filter, "objectSid=") {
				return []*ldapv3.Entry{entry(dn, map[string][]string{
					"sAMAccountName": {"alice"},
					"uidNumber":      {"10001"},
					"gidNumber":      {"10000"},
				}, encodeSID(t, 1104))}, nil
			}
			return nil, nil
		},
	}
	cfg := baseCfg()
	cfg.NestedGroups = false
	p := newTestProvider(t, cfg, fc)
	res, err := p.Resolve(context.Background(), &identity.Credential{ExternalID: "S-1-5-21-1-2-3-1104"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !res.Found || res.UID != 10001 {
		t.Errorf("by-SID resolve failed: %+v", res)
	}
}

func TestCanResolve(t *testing.T) {
	p := newTestProvider(t, baseCfg(), &fakeConn{search: func(*ldapv3.SearchRequest) ([]*ldapv3.Entry, error) { return nil, nil }})
	cases := []struct {
		id   string
		want bool
	}{
		{"alice@DITTOFS.AD", true},
		{"S-1-5-21-1-2-3-1104", true},
		{"alice@OTHER.REALM", false}, // realm mismatch
		{"OWNER@", false},
		{"GROUP@", false},
		{"EVERYONE@", false},
		{"justaname", false},
	}
	for _, tc := range cases {
		if got := p.CanResolve(&identity.Credential{ExternalID: tc.id}); got != tc.want {
			t.Errorf("CanResolve(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
	// Explicit provider routing always claims.
	if !p.CanResolve(&identity.Credential{Provider: ProviderName, ExternalID: "x"}) {
		t.Error("explicit provider routing should be claimed")
	}

	// A credential tagged with a DIFFERENT auth source (e.g. Provider="kerberos"
	// from the NFS/SMB GSS path) must still be claimed by shape: an in-realm
	// principal or an AD SID is resolved against the directory as the fallback,
	// while a foreign-realm principal is left for another provider. This is the
	// fix for the bug where Kerberos-authenticated domain users never reached
	// the directory and resolved to nobody.
	taggedCases := []struct {
		id   string
		want bool
	}{
		{"alice@DITTOFS.AD", true},    // in-realm principal -> claimed
		{"S-1-5-21-1-2-3-1104", true}, // AD SID -> claimed
		{"alice@OTHER.REALM", false},  // foreign realm -> not ours
		{"OWNER@", false},             // NFSv4 special principal -> never
	}
	for _, tc := range taggedCases {
		got := p.CanResolve(&identity.Credential{Provider: "kerberos", ExternalID: tc.id})
		if got != tc.want {
			t.Errorf("CanResolve(kerberos-tagged %q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

func TestNew_RejectsInvalidConfig(t *testing.T) {
	_, err := New(&Config{Enabled: true, URL: "ldap://x", BaseDN: "DC=x", BindDN: "CN=y"}, nil, nil)
	if err == nil {
		t.Fatal("expected New to reject plaintext config")
	}
}

func TestSIDToLDAPFilter_RoundTrips(t *testing.T) {
	s := &sid.SID{
		Revision:            1,
		SubAuthorityCount:   5,
		IdentifierAuthority: [6]byte{0, 0, 0, 0, 0, 5},
		SubAuthorities:      []uint32{21, 1, 2, 3, 1104},
	}
	got := sidToLDAPFilter(s)
	if !strings.HasPrefix(got, "\\01\\05") {
		t.Errorf("unexpected filter prefix: %q", got)
	}
}
