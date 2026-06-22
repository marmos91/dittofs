package smb

import (
	"context"
	"strings"
	"time"

	smbrpc "github.com/marmos91/dittofs/internal/adapter/smb/rpc"
	"github.com/marmos91/dittofs/pkg/identity"
)

// identityForeignSIDResolver bridges the centralized identity.Resolver to the
// LSARPC ForeignSIDResolver interface. It resolves a foreign (AD/LDAP) domain
// SID to its account name + NetBIOS domain by issuing a SID-keyed credential
// to the resolver, which routes it to the LDAP/AD provider's objectSid search.
//
// The resolver already caches forward lookups (positive + negative TTL), so a
// repeated Explorer "Security tab" lookup of the same SID hits the cache rather
// than re-querying the directory.
type identityForeignSIDResolver struct {
	resolver *identity.Resolver

	// netbiosDomain is the AD short domain (e.g. CONTOSO) advertised for
	// resolved foreign accounts. When empty, the domain is derived from the
	// resolved identity's realm/Domain (first DNS label, upper-cased).
	netbiosDomain string

	timeout time.Duration
}

// lsarpcForeignLookupTimeout bounds a single directory round-trip triggered by
// an Explorer Security-tab lookup. Generous for one LDAP search but short
// enough that a hung directory never wedges the named-pipe handler.
const lsarpcForeignLookupTimeout = 5 * time.Second

// newIdentityForeignSIDResolver builds a ForeignSIDResolver backed by the given
// identity.Resolver. netbiosDomain is the configured AD short name (may be
// empty, in which case it is derived from the resolved realm). Returns nil when
// the resolver is nil so callers can pass the result straight through.
func newIdentityForeignSIDResolver(resolver *identity.Resolver, netbiosDomain string) smbrpc.ForeignSIDResolver {
	if resolver == nil {
		return nil
	}
	return &identityForeignSIDResolver{
		resolver:      resolver,
		netbiosDomain: netbiosDomain,
		timeout:       lsarpcForeignLookupTimeout,
	}
}

// LookupSID resolves a canonical SID string to an AD account name + NetBIOS
// domain. The resolver's LDAP/AD provider matches the SID via objectSid and
// returns the sAMAccountName. A miss (Found=false) or any infrastructure error
// returns ok=false so the SID stays unmapped — Explorer then shows the raw SID
// for that entry, never a fault.
func (r *identityForeignSIDResolver) LookupSID(sidString string) (name, domain string, sidType uint16, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()

	resolved, err := r.resolver.Resolve(ctx, &identity.Credential{ExternalID: sidString})
	if err != nil || resolved == nil || !resolved.Found || resolved.Username == "" {
		return "", "", 0, false
	}

	dom := r.netbiosDomain
	if dom == "" {
		dom = netbiosFromRealm(resolved.Domain)
	}

	// The forward directory resolve matches objectClass=user, so a hit is an
	// account (user) rather than a group.
	return resolved.Username, dom, smbrpc.SidTypeUser, true
}

// netbiosFromRealm derives a NetBIOS short domain from a Kerberos realm / DNS
// domain by taking the first label and upper-casing it (CONTOSO.COM -> CONTOSO).
// This is the conventional Samba derivation used only as a fallback when no
// NetBIOS name was configured explicitly; it returns "" for an empty realm.
func netbiosFromRealm(realm string) string {
	realm = strings.TrimSpace(realm)
	if realm == "" {
		return ""
	}
	if idx := strings.IndexByte(realm, '.'); idx > 0 {
		realm = realm[:idx]
	}
	return strings.ToUpper(realm)
}
