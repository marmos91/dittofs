package handlers

import (
	"net"

	"github.com/marmos91/dittofs/internal/adapter/nfs/portmap/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/portmap/xdr"
	"github.com/marmos91/dittofs/internal/logger"
)

// netidToProtocol maps an RPCBIND netid to the v2 IPPROTO value the registry
// keys on. The address family (IPv6 for the "*6" netids) is implied by the
// client's own connection, so it need not be tracked in the registry.
func netidToProtocol(netid string) (uint32, bool) {
	switch netid {
	case "tcp", "tcp6":
		return types.ProtoTCP, true
	case "udp", "udp6":
		return types.ProtoUDP, true
	}
	return 0, false
}

// protocolToNetid renders a registry protocol back to its IPv4 netid for DUMP.
func protocolToNetid(prot uint32) string {
	switch prot {
	case types.ProtoTCP:
		return "tcp"
	case types.ProtoUDP:
		return "udp"
	}
	return ""
}

// uaddrHost extracts the textual host for a universal address from a client
// address. The portmapper binds wildcard, so the address a service is reachable
// at is the same one the client used to reach the portmapper; libtirpc-based
// clients additionally fix up the host to the rpcbind address they queried, so
// the port carried by the uaddr is what is authoritative.
func uaddrHost(clientAddr string) string {
	host, _, err := net.SplitHostPort(clientAddr)
	if err != nil {
		return clientAddr
	}
	return host
}

// Getaddr implements RPCBIND GETADDR / GETVERSADDR (RFC 1833, procedures 3/9).
// It resolves a (prog, vers, netid) tuple to a universal address, returning an
// empty string when the service is not registered (the protocol's "not found").
func (h *Handler) Getaddr(data []byte, clientAddr string) ([]byte, error) {
	rpcb, err := xdr.DecodeRPCB(data)
	if err != nil {
		return nil, err
	}

	proto, ok := netidToProtocol(rpcb.Netid)
	if !ok {
		logger.Debug("RPCBIND GETADDR: unknown netid",
			"netid", rpcb.Netid, "client", clientAddr)
		return xdr.EncodeGetaddrResponse(""), nil
	}

	port := h.Registry.Getport(rpcb.Prog, rpcb.Vers, proto)
	if port == 0 {
		return xdr.EncodeGetaddrResponse(""), nil
	}

	uaddr := xdr.BuildUaddr(uaddrHost(clientAddr), port)
	logger.Debug("RPCBIND GETADDR",
		"prog", rpcb.Prog, "vers", rpcb.Vers, "netid", rpcb.Netid,
		"uaddr", uaddr, "client", clientAddr)
	return xdr.EncodeGetaddrResponse(uaddr), nil
}

// RpcbDump implements RPCBIND DUMP (RFC 1833, procedure 4), returning every
// registration as an rpcb entry with a universal address derived from the
// client's address.
func (h *Handler) RpcbDump(clientAddr string) ([]byte, error) {
	host := uaddrHost(clientAddr)
	mappings := h.Registry.Dump()
	entries := make([]xdr.RPCB, 0, len(mappings))
	for _, m := range mappings {
		netid := protocolToNetid(m.Prot)
		if netid == "" {
			continue
		}
		entries = append(entries, xdr.RPCB{
			Prog:  m.Prog,
			Vers:  m.Vers,
			Netid: netid,
			Addr:  xdr.BuildUaddr(host, m.Port),
			Owner: "superuser",
		})
	}
	return xdr.EncodeRpcbDumpResponse(entries), nil
}
