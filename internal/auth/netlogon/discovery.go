package netlogon

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

// DCInfo identifies a domain controller discovered via DNS SRV records.
type DCInfo struct {
	// FQDN is the DC's canonical DNS host name without the trailing dot,
	// e.g. "dc01.dittofs.ad". It is the basis for the Kerberos service
	// principal (cifs/<FQDN>) used to authenticate the NETLOGON SMB session.
	FQDN string
	// Port is the SRV record port (389 for the _ldap locator record).
	Port uint16
	// Priority/Weight are the SRV selection fields, lowest priority first.
	Priority uint16
	Weight   uint16
}

// SPN returns the SMB service principal name for the DC (cifs/<FQDN>).
//
// AD domain controllers register HOST/<fqdn> rather than an explicit
// cifs/<fqdn>, but the default AD sPNMappings alias the host service class to
// cifs (and a dozen others), so the KDC issues a cifs/<fqdn> ticket from the
// HOST key. This is the same SPN a Windows client uses for an SMB session.
func (d DCInfo) SPN() string { return "cifs/" + d.FQDN }

// DiscoverDCs resolves the domain controllers for realm using the AD DNS
// locator SRV record (_ldap._tcp.dc._msdcs.<realm>). net.LookupSRV already
// returns the records sorted by priority and randomized by weight per RFC 2782,
// and that order is preserved here. This is the discovery half of #1324.
//
// In an Active Directory domain the DC is itself the authoritative DNS server,
// so dnsServer is normally a DC address (we already hold one for connectivity);
// pointing the resolver at it both avoids a dependency on the host's resolv.conf
// and guarantees the records resolve. If dnsServer is empty the system resolver
// is used.
func DiscoverDCs(ctx context.Context, realm, dnsServer string) ([]DCInfo, error) {
	realm = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(realm)), ".")
	if realm == "" {
		return nil, fmt.Errorf("netlogon: discover DC: empty realm")
	}

	resolver := net.DefaultResolver
	if dnsServer != "" {
		resolver = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				d := net.Dialer{Timeout: 5 * time.Second}
				return d.DialContext(ctx, network, net.JoinHostPort(dnsServer, "53"))
			},
		}
	}

	// _ldap._tcp.dc._msdcs.<realm> is the AD "DC locator" record: every DC in
	// the domain publishes it. LookupSRV already returns records sorted by
	// priority then weight per RFC 2782.
	const service, proto = "ldap", "tcp"
	name := "dc._msdcs." + realm
	_, srvs, err := resolver.LookupSRV(ctx, service, proto, name)
	if err != nil {
		return nil, fmt.Errorf("netlogon: SRV lookup _%s._%s.%s: %w", service, proto, name, err)
	}
	if len(srvs) == 0 {
		return nil, fmt.Errorf("netlogon: no DC SRV records for realm %q", realm)
	}

	dcs := make([]DCInfo, 0, len(srvs))
	for _, s := range srvs {
		dcs = append(dcs, DCInfo{
			FQDN:     strings.TrimSuffix(s.Target, "."),
			Port:     s.Port,
			Priority: s.Priority,
			Weight:   s.Weight,
		})
	}
	return dcs, nil
}
