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

// resolveDialTarget returns the address to dial for the NETLOGON SMB session and
// the Kerberos service principal (cifs/<fqdn>) of that exact host. The SPN must
// name the dialed host: a ticket for a different DC would not authenticate the
// SMB session, so the SPN is always tied to the dial target rather than to an
// arbitrary SRV result (#1324). Three cases:
//
//   - No dc_address configured: locate a DC from the realm via DNS SRV (the host
//     resolver, since a domain-joined host's resolv.conf points at a DC) and dial
//     it by name.
//   - dc_address is a hostname: it already names the DC — dial it and derive the
//     SPN directly, no SRV round-trip needed.
//   - dc_address is an IP: query the SRV locator (the DC is its own DNS server)
//     and pick the DC whose address matches the dialed IP. A single-DC domain is
//     unambiguous; if no SRV target matches, fall back to the first SRV result.
func resolveDialTarget(ctx context.Context, mc MachineCredential) (dialServer, spn string, err error) {
	if len(mc.DCAddresses) == 0 {
		dcs, derr := DiscoverDCs(ctx, mc.Realm, "")
		if derr != nil {
			return "", "", fmt.Errorf("discover DC: %w", derr)
		}
		if len(dcs) == 0 {
			return "", "", fmt.Errorf("no DC discovered for realm %q and none configured", mc.Realm)
		}
		return dcs[0].FQDN, dcs[0].SPN(), nil
	}

	dial := mc.DCAddresses[0]
	host := dial
	if h, _, splitErr := net.SplitHostPort(dial); splitErr == nil {
		host = h
	}
	if net.ParseIP(host) == nil {
		// Hostname dial target already names the DC.
		return dial, "cifs/" + host, nil
	}

	// IP dial target: resolve a name for the SPN via the DC's own DNS.
	dcs, derr := DiscoverDCs(ctx, mc.Realm, host)
	if derr != nil {
		return "", "", fmt.Errorf("discover DC: %w", derr)
	}
	if len(dcs) == 0 {
		return "", "", fmt.Errorf("no DC discovered for realm %q", mc.Realm)
	}
	chosen := dcs[0]
	if len(dcs) > 1 {
		if m := matchDCByIP(ctx, dcs, host); m != nil {
			chosen = *m
		}
		// else: ambiguous multi-DC IP — fall back to the first SRV result.
	}
	return dial, chosen.SPN(), nil
}

// matchDCByIP returns the discovered DC whose FQDN resolves (via the DC's own
// DNS server at dnsServer) to dnsServer, or nil if none match. Used to bind the
// Kerberos SPN to the configured dial IP in a multi-DC domain.
func matchDCByIP(ctx context.Context, dcs []DCInfo, dnsServer string) *DCInfo {
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			return d.DialContext(ctx, network, net.JoinHostPort(dnsServer, "53"))
		},
	}
	for i := range dcs {
		addrs, lerr := resolver.LookupHost(ctx, dcs[i].FQDN)
		if lerr != nil {
			continue
		}
		for _, a := range addrs {
			if a == dnsServer {
				return &dcs[i]
			}
		}
	}
	return nil
}
