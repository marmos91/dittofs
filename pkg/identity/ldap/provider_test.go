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

// encodeDomainSID returns the binary objectSid for the domain object
// "S-1-5-21-1-2-3" (four sub-authorities, no RID) — what a base read of the
// domain root returns, and the prefix rid-mode reverse lookup appends a RID to.
func encodeDomainSID(t *testing.T) []byte {
	t.Helper()
	s := &sid.SID{
		Revision:            1,
		SubAuthorityCount:   4,
		IdentifierAuthority: [6]byte{0, 0, 0, 0, 0, 5},
		SubAuthorities:      []uint32{21, 1, 2, 3},
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

// TestResolve_BySID_Group verifies that a group SID (e.g. "Domain Admins",
// RID 512) resolves by objectSid across both object classes and is tagged
// IsGroup, so the LSARPC Security-tab path reports SidTypeGroup instead of
// leaving it "Account Unknown" (the pre-fix user-only filter missed groups).
func TestResolve_BySID_Group(t *testing.T) {
	dn := "CN=Domain Admins,CN=Users,DC=dittofs,DC=ad"
	var gotFilter string
	fc := &fakeConn{
		search: func(req *ldapv3.SearchRequest) ([]*ldapv3.Entry, error) {
			gotFilter = req.Filter
			if strings.Contains(req.Filter, "objectSid=") {
				return []*ldapv3.Entry{entry(dn, map[string][]string{
					"sAMAccountName": {"Domain Admins"},
					"objectClass":    {"top", "group"},
				}, encodeSID(t, 512))}, nil
			}
			return nil, nil
		},
	}
	cfg := baseCfg()
	cfg.NestedGroups = false
	p := newTestProvider(t, cfg, fc)
	res, err := p.Resolve(context.Background(), &identity.Credential{ExternalID: "S-1-5-21-1-2-3-512"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !res.Found || res.Username != "Domain Admins" {
		t.Fatalf("group by-SID resolve failed: %+v", res)
	}
	if !res.IsGroup {
		t.Error("expected IsGroup=true for a group SID resolution")
	}
	// The filter must admit group objects, not just users.
	if !strings.Contains(gotFilter, "objectClass=group") {
		t.Errorf("SID filter does not match groups: %q", gotFilter)
	}
	// idmap:rid → UID/GID derive from the RID (512).
	if res.UID != 512 || res.GID != 512 {
		t.Errorf("UID/GID = %d/%d, want 512/512 (RID)", res.UID, res.GID)
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

// TestLookupUID_RFC2307 covers the reverse uid→name search that backs the
// LSARPC owner-SID display path. The provider must issue a
// (&(objectClass=user)(uidNumber=N)) filter and return the sAMAccountName +
// realm. A miss returns ok=false.
func TestLookupUID_RFC2307(t *testing.T) {
	var sawFilter string
	fc := &fakeConn{
		search: func(req *ldapv3.SearchRequest) ([]*ldapv3.Entry, error) {
			sawFilter = req.Filter
			if strings.Contains(req.Filter, "uidNumber=10001") {
				return []*ldapv3.Entry{entry("CN=alice,DC=dittofs,DC=ad",
					map[string][]string{"sAMAccountName": {"alice"}}, nil)}, nil
			}
			return nil, nil
		},
	}
	p := newTestProvider(t, baseCfg(), fc)

	name, domain, ok := p.LookupUID(context.Background(), 10001)
	if !ok {
		t.Fatal("LookupUID(10001) ok=false, want true")
	}
	if name != "alice" {
		t.Errorf("name = %q, want alice", name)
	}
	if domain != "DITTOFS.AD" {
		t.Errorf("domain = %q, want DITTOFS.AD", domain)
	}
	if !strings.Contains(sawFilter, "objectClass=user") || !strings.Contains(sawFilter, "uidNumber=10001") {
		t.Errorf("filter = %q, want objectClass=user + uidNumber=10001", sawFilter)
	}

	// Miss.
	if _, _, ok := p.LookupUID(context.Background(), 99999); ok {
		t.Error("LookupUID(99999) ok=true, want false (no match)")
	}
}

// TestLookupGID_RFC2307 mirrors TestLookupUID_RFC2307 for the group path.
func TestLookupGID_RFC2307(t *testing.T) {
	var sawFilter string
	fc := &fakeConn{
		search: func(req *ldapv3.SearchRequest) ([]*ldapv3.Entry, error) {
			sawFilter = req.Filter
			if strings.Contains(req.Filter, "gidNumber=10000") {
				return []*ldapv3.Entry{entry("CN=domain users,DC=dittofs,DC=ad",
					map[string][]string{"sAMAccountName": {"domain users"}}, nil)}, nil
			}
			return nil, nil
		},
	}
	p := newTestProvider(t, baseCfg(), fc)

	name, domain, ok := p.LookupGID(context.Background(), 10000)
	if !ok || name != "domain users" || domain != "DITTOFS.AD" {
		t.Errorf("LookupGID(10000) = (%q, %q, %v), want (domain users, DITTOFS.AD, true)", name, domain, ok)
	}
	if !strings.Contains(sawFilter, "objectClass=group") || !strings.Contains(sawFilter, "gidNumber=10000") {
		t.Errorf("filter = %q, want objectClass=group + gidNumber=10000", sawFilter)
	}
}

// TestLookupUID_RIDMode confirms reverse lookup RESOLVES in rid mode by
// reconstructing the account SID (discovered domain SID + UID-as-RID) and
// matching objectSid — so a file owner (e.g. the domain Administrator, RID 500)
// renders as DOMAIN\name in the LSARPC Security-tab display instead of a
// synthetic unix_user:N. The domain SID is discovered once (base read of the
// domain root) and cached across lookups.
func TestLookupUID_RIDMode(t *testing.T) {
	cfg := baseCfg()
	cfg.Idmap = IdmapRID
	// The exact objectSid filter fragment for the reconstructed RID-500 SID, so
	// the mock only "finds" Administrator for RID 500 (a different RID misses).
	want500 := sidToLDAPFilter(sid.ParseSIDMust("S-1-5-21-1-2-3-500"))
	var acctFilter string
	domainReads := 0
	fc := &fakeConn{search: func(req *ldapv3.SearchRequest) ([]*ldapv3.Entry, error) {
		switch {
		// Account search by the reconstructed objectSid (domain SID + RID).
		case strings.Contains(req.Filter, "objectClass=user") && strings.Contains(req.Filter, want500):
			acctFilter = req.Filter
			return []*ldapv3.Entry{entry("CN=Administrator,CN=Users,DC=dittofs,DC=ad",
				map[string][]string{"sAMAccountName": {"Administrator"}}, encodeSID(t, 500))}, nil
		// Domain SID discovery: a base-scoped read of the base DN (the domain root).
		case req.Scope == ldapv3.ScopeBaseObject && req.BaseDN == cfg.BaseDN:
			domainReads++
			return []*ldapv3.Entry{entry(cfg.BaseDN, nil, encodeDomainSID(t))}, nil
		}
		return nil, nil
	}}
	p := newTestProvider(t, cfg, fc)

	name, domain, ok := p.LookupUID(context.Background(), 500)
	if !ok || name != "Administrator" || domain != "DITTOFS.AD" {
		t.Fatalf("LookupUID(500) = (%q, %q, %v), want (Administrator, DITTOFS.AD, true)", name, domain, ok)
	}
	if !strings.Contains(acctFilter, "objectClass=user") || !strings.Contains(acctFilter, "objectSid=") {
		t.Errorf("account filter = %q, want objectClass=user + objectSid=", acctFilter)
	}

	// A second lookup must reuse the cached domain SID (no second base read).
	if _, _, ok := p.LookupUID(context.Background(), 500); !ok {
		t.Error("second LookupUID(500) should still resolve")
	}
	if domainReads != 1 {
		t.Errorf("domain SID read %d times, want 1 (cached after first)", domainReads)
	}

	// An unknown RID still misses cleanly.
	if _, _, ok := p.LookupUID(context.Background(), 99999); ok {
		t.Error("LookupUID(99999) should miss in rid mode")
	}
}

// TestLookupGID_RIDMode mirrors the UID path for a group RID (e.g. Domain
// Admins, RID 512) so a file's group owner resolves in rid mode too.
func TestLookupGID_RIDMode(t *testing.T) {
	cfg := baseCfg()
	cfg.Idmap = IdmapRID
	fc := &fakeConn{search: func(req *ldapv3.SearchRequest) ([]*ldapv3.Entry, error) {
		switch {
		case strings.Contains(req.Filter, "objectSid=") && strings.Contains(req.Filter, "objectClass=group"):
			return []*ldapv3.Entry{entry("CN=Domain Admins,CN=Users,DC=dittofs,DC=ad",
				map[string][]string{"sAMAccountName": {"Domain Admins"}}, encodeSID(t, 512))}, nil
		case req.Scope == ldapv3.ScopeBaseObject && req.BaseDN == cfg.BaseDN:
			return []*ldapv3.Entry{entry(cfg.BaseDN, nil, encodeDomainSID(t))}, nil
		}
		return nil, nil
	}}
	p := newTestProvider(t, cfg, fc)

	name, _, ok := p.LookupGID(context.Background(), 512)
	if !ok || name != "Domain Admins" {
		t.Fatalf("LookupGID(512) = (%q, %v), want (Domain Admins, true)", name, ok)
	}
}

// TestLookupUID_RIDMode_NoDomainSID confirms that when the domain SID cannot be
// discovered, rid-mode reverse lookup degrades to a clean miss (raw SID) rather
// than issuing a malformed objectSid search.
func TestLookupUID_RIDMode_NoDomainSID(t *testing.T) {
	cfg := baseCfg()
	cfg.Idmap = IdmapRID
	acctSearched := false
	fc := &fakeConn{search: func(req *ldapv3.SearchRequest) ([]*ldapv3.Entry, error) {
		if strings.Contains(req.Filter, "objectSid=") {
			acctSearched = true
		}
		// No domain object and no RootDSE defaultNamingContext: every search misses.
		return nil, nil
	}}
	p := newTestProvider(t, cfg, fc)

	if _, _, ok := p.LookupUID(context.Background(), 500); ok {
		t.Error("LookupUID should miss when the domain SID is undiscoverable")
	}
	if acctSearched {
		t.Error("must not issue an account objectSid search without a domain SID")
	}
}

// TestDeriveUIDGID_RFC2307_GroupUsesGidNumber guards the #1617 round-trip fix: a
// group object carries gidNumber but no uidNumber, so the paired uid+gid guard
// misses it. deriveUIDGID must honor the stamped gidNumber rather than falling
// through to the RID — otherwise resolving a group SID back to a GID (Group SD
// round-trip) would recover the RID and silently rewrite a file's gid.
func TestDeriveUIDGID_RFC2307_GroupUsesGidNumber(t *testing.T) {
	p, err := New(baseCfg(), nil, nil) // baseCfg() is IdmapRFC2307
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Group entry: objectClass=group, gidNumber present, NO uidNumber, RID 1105.
	e := entry("CN=devs,CN=Users,DC=dittofs,DC=ad", map[string][]string{
		"objectClass": {"top", "group"},
		"gidNumber":   {"10010"},
	}, encodeSID(t, 1105))

	_, gid, err := p.deriveUIDGID(e)
	if err != nil {
		t.Fatalf("deriveUIDGID: %v", err)
	}
	if gid != 10010 {
		t.Errorf("gid = %d, want 10010 (gidNumber, not RID 1105)", gid)
	}
}
