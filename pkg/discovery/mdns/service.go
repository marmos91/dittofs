// Package mdns implements a minimal multicast-DNS / DNS-SD responder
// (RFC 6762 / RFC 6763) that advertises DittoFS's SMB and NFS services so they
// appear in macOS Finder and Linux/Avahi file browsers. It is a from-scratch
// responder built on golang.org/x/net/dns/dnsmessage — it advertises a fixed
// set of locally-owned service records and answers queries for them; it does
// not browse or resolve other hosts' services.
//
// A single process-global Responder owns the one 224.0.0.251:5353 socket; each
// protocol adapter registers only its own ServiceRecords through it, so the two
// adapters share one responder and one host record set (see responder.go).
package mdns

import "net"

// DefaultDomain is the DNS-SD domain for link-local multicast DNS.
const DefaultDomain = "local"

// ServiceRecord describes one DNS-SD service instance to advertise, e.g. the
// SMB service on this host. It is the unit an adapter registers with the
// Responder.
type ServiceRecord struct {
	// Instance is the service instance label, typically the server name
	// (e.g. "DITTOFS"). It is a single DNS label; dots are not supported.
	Instance string

	// Service is the DNS-SD service type, e.g. "_smb._tcp" or "_nfs._tcp".
	Service string

	// Domain defaults to DefaultDomain ("local") when empty.
	Domain string

	// Port is the TCP port the service listens on (the adapter's real bound
	// port — 445/12445 for SMB, 12049 for NFS).
	Port uint16

	// TXT holds DNS-SD TXT record strings, e.g. {"path=/ditto"} or
	// {"model=RackMac"}. When empty, a single zero-length string is advertised
	// (an empty but present TXT record, as DNS-SD requires).
	TXT []string

	// IPv4/IPv6 are the host addresses advertised in A/AAAA records for the
	// service's target host. When both are empty the responder fills them from
	// the host's interfaces at registration time.
	IPv4 []net.IP
	IPv6 []net.IP
}

func (s ServiceRecord) domain() string {
	if s.Domain == "" {
		return DefaultDomain
	}
	return s.Domain
}

// instanceName is the fully-qualified DNS-SD instance name,
// e.g. "DITTOFS._smb._tcp.local."
func (s ServiceRecord) instanceName() string {
	return s.Instance + "." + s.Service + "." + s.domain() + "."
}

// serviceName is the fully-qualified service type,
// e.g. "_smb._tcp.local." — the name browsers query with a PTR.
func (s ServiceRecord) serviceName() string {
	return s.Service + "." + s.domain() + "."
}

// hostName is the target host name for the SRV/A/AAAA records,
// e.g. "DITTOFS.local."
func (s ServiceRecord) hostName() string {
	return s.Instance + "." + s.domain() + "."
}

// metaName is the DNS-SD service-type enumeration name,
// e.g. "_services._dns-sd._udp.local."
func (s ServiceRecord) metaName() string {
	return "_services._dns-sd._udp." + s.domain() + "."
}

// txtStrings returns the TXT strings to advertise, guaranteeing at least one
// (possibly empty) string so the TXT record is present per DNS-SD.
func (s ServiceRecord) txtStrings() []string {
	if len(s.TXT) == 0 {
		return []string{""}
	}
	return s.TXT
}
