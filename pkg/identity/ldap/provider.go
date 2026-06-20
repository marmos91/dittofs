// Package ldap provides an LDAP/Active Directory identity provider for DittoFS.
//
// It resolves AD principals — either a "user@REALM" form or an AD SID — to a
// DittoFS Unix identity by querying the directory over an encrypted connection
// (LDAPS or StartTLS). It reads RFC2307 POSIX attributes (uidNumber/gidNumber)
// when present, or falls back to algorithmic RID-based mapping, and resolves
// the user's (optionally nested) group memberships.
//
// Security: the provider refuses to bind over a plaintext connection unless the
// configuration explicitly opts in (Config.AllowPlaintext). The default posture
// requires LDAPS or a StartTLS upgrade.
//
// The provider implements identity.IdentityProvider and mirrors the structure
// of pkg/identity/kerberos.
package ldap

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	ldapv3 "github.com/go-ldap/ldap/v3"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/auth/sid"
	"github.com/marmos91/dittofs/pkg/identity"
)

const ProviderName = "ldap"

// matchingRuleInChain is the AD LDAP_MATCHING_RULE_IN_CHAIN OID. Applied to
// memberOf it returns the transitive (nested) set of groups a user belongs to,
// resolved server-side by the DC.
const matchingRuleInChain = "1.2.840.113556.1.4.1941"

// dialer abstracts the LDAP connection for testability. The production
// implementation wraps github.com/go-ldap/ldap/v3.
type conn interface {
	Bind(username, password string) error
	Search(req *ldapv3.SearchRequest) (*ldapv3.SearchResult, error)
	Close() error
}

// connectFunc opens a bound connection to the directory. Swapped in tests.
type connectFunc func(ctx context.Context, cfg *Config) (conn, error)

// Provider resolves AD principals/SIDs to DittoFS Unix identities over LDAP.
// Implements identity.IdentityProvider. Safe for concurrent use.
type Provider struct {
	cfg     *Config
	connect connectFunc

	// store optionally short-circuits resolution with an admin-configured
	// external-ID → username link before querying the directory.
	store      identity.LinkStore
	userLookup identity.UserLookup
}

// New constructs an LDAP identity provider from a validated Config.
//
//   - cfg: LDAP/AD connection + idmap configuration (must be Enabled + valid)
//   - store: optional link store for admin-configured externalID → username
//     overrides (may be nil)
//   - userLookup: optional callback to resolve an admin-linked username to a
//     DittoFS identity (may be nil; only used for the link-override path)
func New(cfg *Config, store identity.LinkStore, userLookup identity.UserLookup) (*Provider, error) {
	if cfg == nil {
		return nil, fmt.Errorf("ldap: nil config")
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Provider{
		cfg:        cfg,
		connect:    dialAndBind,
		store:      store,
		userLookup: userLookup,
	}, nil
}

func (p *Provider) Name() string { return ProviderName }

// CanResolve reports whether this provider should handle the credential.
// It claims AD SIDs (S-1-...) and "user@REALM" principals matching the
// configured realm. The NFSv4 special principals are never claimed.
func (p *Provider) CanResolve(cred *identity.Credential) bool {
	if cred.Provider != "" {
		return cred.Provider == ProviderName
	}
	switch cred.ExternalID {
	case "OWNER@", "GROUP@", "EVERYONE@":
		return false
	}
	if strings.HasPrefix(cred.ExternalID, "S-1-") {
		return true
	}
	name, realm := splitPrincipal(cred.ExternalID)
	if name == "" || realm == "" {
		return false
	}
	// When a realm is configured, only claim principals in that realm so the
	// chain can fall through to other providers for foreign realms.
	if p.cfg.Realm != "" {
		return strings.EqualFold(realm, p.cfg.Realm)
	}
	return true
}

// Resolve maps an AD credential to a DittoFS Unix identity.
//
// Resolution order:
//  1. Admin-configured link (LinkStore) → userLookup, if both present.
//  2. Directory query: bind as the service account, find the user object by
//     sAMAccountName (or DN/SID), read RFC2307 UID/GID (or RID fallback), and
//     resolve the user's (optionally nested) group memberships.
//
// Found=false is returned for a valid-but-unmapped credential; an error is
// returned only for infrastructure failures (dial/bind/search).
func (p *Provider) Resolve(ctx context.Context, cred *identity.Credential) (*identity.ResolvedIdentity, error) {
	// 1. Admin-configured override link.
	if p.store != nil && p.userLookup != nil {
		username, found, err := p.store.GetLink(ctx, ProviderName, cred.ExternalID)
		if err != nil {
			return nil, err
		}
		if found {
			resolved, err := p.userLookup(ctx, username)
			if err != nil {
				return nil, err
			}
			if resolved != nil && resolved.Found {
				out := *resolved
				if out.Domain == "" {
					out.Domain = p.cfg.Realm
				}
				return &out, nil
			}
		}
	}

	// 2. Directory query.
	c, err := p.connect(ctx, p.cfg)
	if err != nil {
		return nil, fmt.Errorf("ldap: connect/bind: %w", err)
	}
	defer func() { _ = c.Close() }()

	filter, err := p.userFilter(cred)
	if err != nil {
		return nil, err
	}

	attrs := []string{"sAMAccountName", "uidNumber", "gidNumber", "objectSid", "primaryGroupID", "memberOf", "distinguishedName"}
	req := ldapv3.NewSearchRequest(
		p.cfg.BaseDN,
		ldapv3.ScopeWholeSubtree, ldapv3.NeverDerefAliases, 2, int(p.cfg.Timeout.Seconds()), false,
		filter, attrs, nil,
	)
	res, err := c.Search(req)
	if err != nil {
		return nil, fmt.Errorf("ldap: user search: %w", err)
	}
	if len(res.Entries) == 0 {
		logger.Debug("ldap: no user matched", "filter", filter, "base", p.cfg.BaseDN)
		return &identity.ResolvedIdentity{Found: false}, nil
	}
	entry := res.Entries[0]

	username := entry.GetAttributeValue("sAMAccountName")
	uid, gid, err := p.deriveUIDGID(entry)
	if err != nil {
		return nil, err
	}

	gids, err := p.resolveGroupGIDs(ctx, c, entry, gid)
	if err != nil {
		return nil, err
	}

	return &identity.ResolvedIdentity{
		Username: username,
		UID:      uid,
		GID:      gid,
		GIDs:     gids,
		Domain:   p.cfg.Realm,
		Found:    true,
	}, nil
}

// userFilter builds an LDAP filter matching the credential. SIDs are matched on
// objectSid; principals on the configured user attribute (sAMAccountName).
func (p *Provider) userFilter(cred *identity.Credential) (string, error) {
	if strings.HasPrefix(cred.ExternalID, "S-1-") {
		s, err := sid.ParseSIDString(cred.ExternalID)
		if err != nil {
			return "", fmt.Errorf("ldap: parse SID %q: %w", cred.ExternalID, err)
		}
		return fmt.Sprintf("(&(objectClass=user)(objectSid=%s))", sidToLDAPFilter(s)), nil
	}
	name, _ := splitPrincipal(cred.ExternalID)
	if name == "" {
		name = cred.ExternalID
	}
	return fmt.Sprintf("(&(objectClass=user)(%s=%s))", p.cfg.UserAttr, ldapv3.EscapeFilter(name)), nil
}

// deriveUIDGID returns the user's POSIX UID/GID according to the idmap mode.
//
//   - rfc2307: read uidNumber/gidNumber. Missing attributes fall through to the
//     RID-based derivation so a partially-provisioned object still resolves.
//   - rid: derive from the object SID's RID (idmap_rid model).
func (p *Provider) deriveUIDGID(entry *ldapv3.Entry) (uint32, uint32, error) {
	if p.cfg.Idmap == IdmapRFC2307 {
		uidStr := entry.GetAttributeValue("uidNumber")
		gidStr := entry.GetAttributeValue("gidNumber")
		if uidStr != "" && gidStr != "" {
			uid, err := parseUint32(uidStr)
			if err != nil {
				return 0, 0, fmt.Errorf("ldap: parse uidNumber %q: %w", uidStr, err)
			}
			gid, err := parseUint32(gidStr)
			if err != nil {
				return 0, 0, fmt.Errorf("ldap: parse gidNumber %q: %w", gidStr, err)
			}
			return uid, gid, nil
		}
		logger.Debug("ldap: RFC2307 attrs absent, falling back to RID idmap",
			"dn", entry.DN)
	}

	rid, err := ridFromEntry(entry)
	if err != nil {
		return 0, 0, err
	}
	// idmap_rid identity: UID == RID. The primary GID defaults to the same RID
	// when no group context is available; resolveGroupGIDs overrides GID with
	// the primaryGroupID-derived value when present.
	return rid, rid, nil
}

// resolveGroupGIDs returns the user's supplementary GIDs. With NestedGroups,
// the AD LDAP_MATCHING_RULE_IN_CHAIN matching rule resolves transitive
// membership server-side; otherwise only the direct memberOf groups are used.
// The primary GID is always included.
func (p *Provider) resolveGroupGIDs(ctx context.Context, c conn, userEntry *ldapv3.Entry, primaryGID uint32) ([]uint32, error) {
	groupDNs, err := p.groupDNs(c, userEntry)
	if err != nil {
		return nil, err
	}

	seenGID := map[uint32]struct{}{primaryGID: {}}
	gids := []uint32{primaryGID}

	for _, dn := range groupDNs {
		gid, ok, err := p.groupGID(c, dn)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if _, dup := seenGID[gid]; dup {
			continue
		}
		seenGID[gid] = struct{}{}
		gids = append(gids, gid)
	}
	return gids, nil
}

// groupDNs returns the DNs of the groups the user belongs to. When nested
// resolution is on, it queries with LDAP_MATCHING_RULE_IN_CHAIN; otherwise it
// reads the direct memberOf list from the user entry.
func (p *Provider) groupDNs(c conn, userEntry *ldapv3.Entry) ([]string, error) {
	if !p.cfg.NestedGroups {
		return userEntry.GetAttributeValues("memberOf"), nil
	}

	filter := fmt.Sprintf("(member:%s:=%s)", matchingRuleInChain, ldapv3.EscapeFilter(userEntry.DN))
	req := ldapv3.NewSearchRequest(
		p.cfg.BaseDN,
		ldapv3.ScopeWholeSubtree, ldapv3.NeverDerefAliases, 0, int(p.cfg.Timeout.Seconds()), false,
		fmt.Sprintf("(&(objectClass=group)%s)", filter),
		[]string{"distinguishedName"}, nil,
	)
	res, err := c.Search(req)
	if err != nil {
		return nil, fmt.Errorf("ldap: nested group search: %w", err)
	}
	dns := make([]string, 0, len(res.Entries))
	for _, e := range res.Entries {
		dns = append(dns, e.DN)
	}
	return dns, nil
}

// groupGID returns the POSIX GID of a group DN. With rfc2307 it reads
// gidNumber; otherwise (or when gidNumber is absent) it derives the GID from
// the group's RID.
func (p *Provider) groupGID(c conn, dn string) (uint32, bool, error) {
	req := ldapv3.NewSearchRequest(
		dn,
		ldapv3.ScopeBaseObject, ldapv3.NeverDerefAliases, 1, int(p.cfg.Timeout.Seconds()), false,
		"(objectClass=group)",
		[]string{"gidNumber", "objectSid"}, nil,
	)
	res, err := c.Search(req)
	if err != nil {
		return 0, false, fmt.Errorf("ldap: group lookup %s: %w", dn, err)
	}
	if len(res.Entries) == 0 {
		return 0, false, nil
	}
	e := res.Entries[0]

	if p.cfg.Idmap == IdmapRFC2307 {
		if g := e.GetAttributeValue("gidNumber"); g != "" {
			gid, err := parseUint32(g)
			if err != nil {
				return 0, false, fmt.Errorf("ldap: parse group gidNumber %q: %w", g, err)
			}
			return gid, true, nil
		}
	}

	rid, err := ridFromEntry(e)
	if err != nil {
		// A group with neither gidNumber nor a decodable SID is skipped rather
		// than failing the whole resolution.
		logger.Debug("ldap: group has no gidNumber or decodable SID, skipping", "dn", dn, "error", err)
		return 0, false, nil
	}
	return rid, true, nil
}

// dialAndBind opens an encrypted connection and binds the service account.
// LDAPS connects with implicit TLS; ldap:// connects plaintext and upgrades via
// StartTLS unless plaintext is explicitly allowed.
func dialAndBind(ctx context.Context, cfg *Config) (conn, error) {
	host, err := hostFromURL(cfg.URL)
	if err != nil {
		return nil, err
	}

	var opts []ldapv3.DialOpt
	if cfg.IsLDAPS() {
		tlsCfg, err := cfg.tlsClientConfig(host)
		if err != nil {
			return nil, err
		}
		opts = append(opts, ldapv3.DialWithTLSConfig(tlsCfg))
	}

	l, err := ldapv3.DialURL(cfg.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", cfg.URL, err)
	}
	if cfg.Timeout > 0 {
		l.SetTimeout(cfg.Timeout)
	}

	if !cfg.IsLDAPS() {
		if cfg.StartTLS {
			tlsCfg, err := cfg.tlsClientConfig(host)
			if err != nil {
				_ = l.Close()
				return nil, err
			}
			if err := l.StartTLS(tlsCfg); err != nil {
				_ = l.Close()
				return nil, fmt.Errorf("ldap StartTLS upgrade failed: %w", err)
			}
		} else if !cfg.AllowPlaintext {
			// Defense-in-depth: Validate() already rejects this combination, but
			// guard here too so a hand-constructed Config can never bind in clear.
			_ = l.Close()
			return nil, fmt.Errorf("ldap: refusing to bind over plaintext ldap:// (set start_tls or allow_plaintext)")
		}
	}

	if err := l.Bind(cfg.BindDN, cfg.BindPassword); err != nil {
		_ = l.Close()
		return nil, fmt.Errorf("ldap bind as %s: %w", cfg.BindDN, err)
	}
	return l, nil
}

// hostFromURL extracts the host (without port) from an ldap(s):// URL, used as
// the TLS ServerName for certificate verification.
func hostFromURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("ldap: parse url %q: %w", raw, err)
	}
	return u.Hostname(), nil
}

// splitPrincipal splits "user@REALM" into ("user", "REALM"). NFSv4 special
// principals and non-principal strings return ("", "").
func splitPrincipal(s string) (name, realm string) {
	switch s {
	case "OWNER@", "GROUP@", "EVERYONE@":
		return "", ""
	}
	idx := strings.LastIndex(s, "@")
	if idx <= 0 || idx == len(s)-1 {
		return "", ""
	}
	return s[:idx], s[idx+1:]
}

// ridFromEntry extracts the RID (final SID sub-authority) from an entry's
// binary objectSid attribute.
func ridFromEntry(e *ldapv3.Entry) (uint32, error) {
	raw := e.GetRawAttributeValue("objectSid")
	if len(raw) == 0 {
		return 0, fmt.Errorf("ldap: entry %s has no objectSid", e.DN)
	}
	s, _, err := sid.DecodeSID(raw)
	if err != nil {
		return 0, fmt.Errorf("ldap: decode objectSid for %s: %w", e.DN, err)
	}
	if len(s.SubAuthorities) == 0 {
		return 0, fmt.Errorf("ldap: objectSid for %s has no sub-authorities", e.DN)
	}
	return s.SubAuthorities[len(s.SubAuthorities)-1], nil
}

// sidToLDAPFilter renders a SID as the backslash-escaped binary form used in an
// LDAP objectSid filter (e.g. "\01\05\00\00...").
func sidToLDAPFilter(s *sid.SID) string {
	var b strings.Builder
	encodeByte := func(v byte) {
		b.WriteByte('\\')
		const hexdigits = "0123456789abcdef"
		b.WriteByte(hexdigits[v>>4])
		b.WriteByte(hexdigits[v&0x0f])
	}
	encodeByte(s.Revision)
	encodeByte(s.SubAuthorityCount)
	for _, ab := range s.IdentifierAuthority {
		encodeByte(ab)
	}
	for _, sa := range s.SubAuthorities {
		// Sub-authorities are little-endian on the wire.
		encodeByte(byte(sa))
		encodeByte(byte(sa >> 8))
		encodeByte(byte(sa >> 16))
		encodeByte(byte(sa >> 24))
	}
	return b.String()
}

func parseUint32(s string) (uint32, error) {
	v, err := strconv.ParseUint(strings.TrimSpace(s), 10, 32)
	if err != nil {
		return 0, err
	}
	return uint32(v), nil
}
