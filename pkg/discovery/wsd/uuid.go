// Package wsd implements a minimal WS-Discovery host responder so DittoFS's SMB
// service appears in the Windows Explorer "Network" view. It is a from-scratch
// implementation of the subset of the OASIS WS-Discovery protocol that Windows
// Function Discovery uses — the same job the external wsdd/wsdd2 daemons do for
// Samba:
//
//   - a UDP multicast responder on 239.255.255.250:3702 that emits Hello on
//     start / Bye on stop and answers Probe/Resolve with ProbeMatch/ResolveMatch
//     (responder.go), and
//   - an HTTP metadata endpoint on tcp/5357 that answers the WS-Transfer Get
//     Windows issues to fetch device metadata, returning a pub:Computer
//     relationship so Explorer renders the host as a Computer (metadata.go).
//
// WS-Discovery is SMB-only: Windows does not browse NFS servers.
package wsd

import (
	"github.com/google/uuid"

	"github.com/marmos91/dittofs/pkg/discovery/hostinfo"
)

// dittofsNamespace is a fixed namespace UUID used to derive a stable per-host
// endpoint UUID (RFC 4122 v5). Any constant works; this one is DittoFS-specific
// (the node segment encodes issue 1609).
var dittofsNamespace = uuid.MustParse("0d117f5d-1609-4000-8000-000000001609")

// EndpointUUID returns the stable "urn:uuid:…" endpoint reference for this host,
// derived from its server name so Windows dedupes the entry across restarts.
func EndpointUUID() string {
	return endpointUUIDFor(hostinfo.ServerName())
}

// endpointUUIDFor is the pure core of EndpointUUID, testable without the real
// hostname.
func endpointUUIDFor(serverName string) string {
	u := uuid.NewSHA1(dittofsNamespace, []byte(serverName))
	return "urn:uuid:" + u.String()
}

// MessageID returns a fresh random "urn:uuid:…" for a single message's
// wsa:MessageID header.
func MessageID() string {
	return "urn:uuid:" + uuid.NewString()
}
