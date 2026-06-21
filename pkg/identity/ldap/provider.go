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

// matchingRuleInChain is the AD LDAP_MATCHING_RULE_IN_CHAIN OID. Applied to the
// group `member` attribute in a group search (member:<oid>:=<userDN>), it
// returns the transitive (nested) set of groups a user belongs to, resolved
// server-side by the DC.
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
	// Explicitly addressed to the LDAP provider.
	if cred.Provider == ProviderName {
		return true
	}
	// An AD object SID is always ours, whatever auth source produced it.
	if strings.HasPrefix(cred.ExternalID, "S-1-") {
		return true
	}
	// A Kerberos/AD principal "user@REALM" in our realm: claim it as the
	// directory backing even when the credential carries a different auth-source
	// tag (e.g. Provider="kerberos" from the NFS/SMB GSS path). The Kerberos
	// provider resolves principals that map to a local DittoFS user; a domain
	// principal with no local account falls through to here so it still resolves
	// to its RFC2307 UID/GID. splitPrincipal rejects the NFSv4 special
	// principals (OWNER@/GROUP@/EVERYONE@) by returning empty name/realm.
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
	if len(res.Entries) > 1 {
		// SizeLimit caps this at 2, so >1 means the lookup is ambiguous. The
		// first entry is used but the resolved identity is non-deterministic —
		// surface it rather than silently picking one.
		logger.Warn("ldap: ambiguous user lookup — multiple entries matched, using first",
			"filter", filter, "base", p.cfg.BaseDN, "count", len(res.Entries))
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
	// idmap_rid identity: UID == RID. With no RFC2307 gidNumber the GID defaults
	// to the same RID; the user's actual AD primary group (primaryGroupID) is
	// added to the supplementary set by resolveGroupGIDs.
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

	// AD does not list a user's primary group in memberOf, so resolve it from
	// primaryGroupID and add it explicitly. Without this the primary group's GID
	// is silently missing from the supplementary set (causes access-denied on
	// files owned by that group in RFC2307 deployments).
	if pgid, ok, err := p.primaryGroupGID(c, userEntry); err != nil {
		return nil, err
	} else if ok {
		if _, dup := seenGID[pgid]; !dup {
			seenGID[pgid] = struct{}{}
			gids = append(gids, pgid)
		}
	}

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
		// SizeLimit = MaxGroupResults bounds the transitive membership the server
		// returns, capping the per-group GID lookups that follow.
		ldapv3.ScopeWholeSubtree, ldapv3.NeverDerefAliases, p.cfg.MaxGroupResults, int(p.cfg.Timeout.Seconds()), false,
		fmt.Sprintf("(&(objectClass=group)%s)", filter),
		[]string{"distinguishedName"}, nil,
	)
	res, err := c.Search(req)
	if err != nil {
		// A size-limit hit is not fatal: use the (capped) partial result and warn
		// rather than failing the whole authentication.
		if res != nil && ldapv3.IsErrorWithCode(err, ldapv3.LDAPResultSizeLimitExceeded) {
			logger.Warn("ldap: nested group membership exceeds max_group_results, truncating",
				"dn", userEntry.DN, "max", p.cfg.MaxGroupResults)
		} else {
			return nil, fmt.Errorf("ldap: nested group search: %w", err)
		}
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

// primaryGroupGID resolves the GID of the user's AD primary group, derived from
// the primaryGroupID RID combined with the user's domain SID. Returns ok=false
// (no error) when the attributes are absent or malformed, so a missing primary
// group never fails resolution.
func (p *Provider) primaryGroupGID(c conn, userEntry *ldapv3.Entry) (uint32, bool, error) {
	pgStr := userEntry.GetAttributeValue("primaryGroupID")
	if pgStr == "" {
		return 0, false, nil
	}
	primaryRID, err := parseUint32(pgStr)
	if err != nil {
		logger.Debug("ldap: malformed primaryGroupID, skipping", "dn", userEntry.DN, "value", pgStr)
		return 0, false, nil
	}

	// In rid mode the GID is the RID directly — no directory round-trip needed.
	if p.cfg.Idmap == IdmapRID {
		return primaryRID, true, nil
	}

	raw := userEntry.GetRawAttributeValue("objectSid")
	if len(raw) == 0 {
		return 0, false, nil
	}
	userSID, _, err := sid.DecodeSID(raw)
	if err != nil || len(userSID.SubAuthorities) == 0 {
		return 0, false, nil
	}
	// Primary group SID = user's domain SID with the trailing RID replaced by
	// primaryGroupID.
	groupSID := *userSID
	subs := append([]uint32(nil), userSID.SubAuthorities...)
	subs[len(subs)-1] = primaryRID
	groupSID.SubAuthorities = subs

	return p.groupGIDBySID(c, &groupSID)
}

// groupGIDBySID resolves a group's POSIX GID from its SID (rfc2307 gidNumber,
// else the group's RID). Returns ok=false when no group matches.
func (p *Provider) groupGIDBySID(c conn, s *sid.SID) (uint32, bool, error) {
	req := ldapv3.NewSearchRequest(
		p.cfg.BaseDN,
		ldapv3.ScopeWholeSubtree, ldapv3.NeverDerefAliases, 1, int(p.cfg.Timeout.Seconds()), false,
		fmt.Sprintf("(&(objectClass=group)(objectSid=%s))", sidToLDAPFilter(s)),
		[]string{"gidNumber", "objectSid"}, nil,
	)
	res, err := c.Search(req)
	if err != nil {
		return 0, false, fmt.Errorf("ldap: primary group lookup: %w", err)
	}
	if len(res.Entries) == 0 {
		return 0, false, nil
	}
	e := res.Entries[0]
	if g := e.GetAttributeValue("gidNumber"); g != "" {
		gid, err := parseUint32(g)
		if err != nil {
			return 0, false, fmt.Errorf("ldap: parse primary group gidNumber %q: %w", g, err)
		}
		return gid, true, nil
	}
	rid, err := ridFromEntry(e)
	if err != nil {
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
