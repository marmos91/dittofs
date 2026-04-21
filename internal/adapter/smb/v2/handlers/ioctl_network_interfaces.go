package handlers

import (
	"bytes"
	"encoding/binary"
	"net"

	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
)

// Conservative placeholder advertised in NETWORK_INTERFACE_INFO.LinkSpeed.
// Clients use this only as a hint for per-channel allocation, not correctness
// (MS-SMB2 §3.2.5.14.11). Reporting an honest link speed would require
// platform-specific code (e.g. /sys/class/net on Linux) for marginal benefit,
// especially under containerized deployments where the host NIC speed is not
// the effective bandwidth anyway. Samba falls back to this same value when
// the kernel cannot report a speed.
const networkInterfaceLinkSpeedBps uint64 = 1_000_000_000

// MS-SMB2 §2.2.32.5.1 — family values (stored in SockAddr_Storage[0:2]).
const (
	sockAddrFamilyINet  uint16 = 0x0002
	sockAddrFamilyINet6 uint16 = 0x0017
)

// Per MS-SMB2 §2.2.32.5, each NETWORK_INTERFACE_INFO entry is 152 bytes:
// Next(4) + IfIndex(4) + Capability(4) + Reserved(4) + LinkSpeed(8) +
// SockAddr_Storage(128).
const networkInterfaceInfoEntrySize = 152

// handleQueryNetworkInterfaceInfo responds to FSCTL_QUERY_NETWORK_INTERFACE_INFO
// (MS-SMB2 §2.2.32.5). The request uses the all-0xFF FileID sentinel and
// carries no meaningful input; the response is a linked list of
// NETWORK_INTERFACE_INFO entries describing the server's usable addresses.
//
// Each entry carries (offset-within-entry):
//
//	 0  Next              u32   Offset in bytes to the next entry; 0 marks the last.
//	 4  IfIndex           u32   Interface index.
//	 8  Capability        u32   Bitmask (RSS/RDMA). We report 0 (neither).
//	12  Reserved          u32   Must be 0.
//	16  LinkSpeed         u64   Advertised speed in bits/sec.
//	24  SockAddr_Storage  128B  family | data | padding per family rules.
//
// IPv6 link-local addresses are skipped: they require the interface index to
// be meaningful in a SOCKADDR_IN6 scope_id, and DittoFS is not advertising
// scoped addresses. Loopback is also skipped — a client that reached the
// server via an external address cannot usefully open a second channel to
// 127.0.0.1.
func (h *Handler) handleQueryNetworkInterfaceInfo(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	fileID, ok := parseIoctlFileID(body)
	if !ok {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}
	// MS-SMB2 §3.3.5.15.13: FileID must be the all-0xFF sentinel.
	if !bytes.Equal(fileID[:], allFFFileID) {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	entries := collectNetworkInterfaceEntries()
	logger.Debug("IOCTL FSCTL_QUERY_NETWORK_INTERFACE_INFO",
		"interfaces", len(entries))
	// MS-SMB2 §3.3.5.15.13: if the server cannot report usable interfaces
	// (enumeration failed, or only loopback/link-local exist), return
	// STATUS_NOT_SUPPORTED so the client cleanly falls back to single-
	// channel instead of parsing a zero-entry list.
	if len(entries) == 0 {
		return NewErrorResult(types.StatusNotSupported), nil
	}

	output := encodeNetworkInterfaceInfoList(entries)
	resp := buildIoctlResponse(FsctlQueryNetworkInterfInfo, fileID, output)
	return NewResult(types.StatusSuccess, resp), nil
}

// networkInterfaceEntry is the parsed subset of a host interface address we
// need to emit a NETWORK_INTERFACE_INFO.
type networkInterfaceEntry struct {
	IfIndex uint32
	IP      net.IP
}

// collectNetworkInterfaceEntries walks host interfaces and returns one entry
// per usable unicast address. Errors from Interfaces() / Addrs() are logged
// and skipped — returning an empty list is preferable to failing the IOCTL.
func collectNetworkInterfaceEntries() []networkInterfaceEntry {
	ifaces, err := net.Interfaces()
	if err != nil {
		logger.Debug("IOCTL network interface enumeration failed",
			"error", err)
		return nil
	}

	var out []networkInterfaceEntry
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ip := ipFromAddr(addr)
			// Skip link-local: we cannot emit a meaningful scope_id in the
			// SOCKADDR_IN6 we serialize.
			if ip == nil || ip.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, networkInterfaceEntry{
				IfIndex: uint32(iface.Index),
				IP:      ip,
			})
		}
	}
	return out
}

// ipFromAddr narrows net.Addr (returned by net.Interface.Addrs) to a usable
// net.IP. Addresses on Linux/Darwin come as *net.IPNet; we accept *net.IPAddr
// defensively too.
func ipFromAddr(a net.Addr) net.IP {
	switch v := a.(type) {
	case *net.IPNet:
		return v.IP
	case *net.IPAddr:
		return v.IP
	}
	return nil
}

// encodeNetworkInterfaceInfoList serializes entries per MS-SMB2 §2.2.32.5.
// Entries are laid out sequentially; each entry's Next field is the offset to
// the start of the following entry (152 bytes), or 0 on the last.
func encodeNetworkInterfaceInfoList(entries []networkInterfaceEntry) []byte {
	if len(entries) == 0 {
		return nil
	}
	w := smbenc.NewWriter(len(entries) * networkInterfaceInfoEntrySize)
	for i, e := range entries {
		var next uint32
		if i < len(entries)-1 {
			next = networkInterfaceInfoEntrySize
		}
		w.WriteUint32(next)
		w.WriteUint32(e.IfIndex)
		w.WriteUint32(0) // Capability — no RSS/RDMA advertised
		w.WriteUint32(0) // Reserved
		w.WriteUint64(networkInterfaceLinkSpeedBps)
		w.WriteBytes(encodeSockAddrStorage(e.IP))
	}
	return w.Bytes()
}

// encodeSockAddrStorage serializes a SOCKADDR_STORAGE per MS-SMB2 §2.2.32.5.1
// into a fixed 128-byte buffer. Integer header fields (Family, Port,
// FlowInfo, ScopeId) are little-endian on the wire per SMB2 convention; IP
// address bytes are already network order in net.IP and copied verbatim.
//
// SOCKADDR_IN (IPv4, 16 used bytes, padded to 128):
//
//	0  Family   u16   0x0002
//	2  Port     u16   0 (unused for interface enumeration)
//	4  IPv4     4B    address bytes (network order)
//	8  Reserved 8B    zero
//
// SOCKADDR_IN6 (IPv6, 28 used bytes, padded to 128):
//
//	 0  Family     u16  0x0017
//	 2  Port       u16  0
//	 4  FlowInfo   u32  0
//	 8  IPv6       16B  address bytes (network order)
//	24  ScopeId    u32  0 (link-local skipped upstream)
func encodeSockAddrStorage(ip net.IP) []byte {
	buf := make([]byte, 128)
	if v4 := ip.To4(); v4 != nil {
		binary.LittleEndian.PutUint16(buf[0:2], sockAddrFamilyINet)
		copy(buf[4:8], v4)
		return buf
	}
	v6 := ip.To16()
	if v6 == nil {
		return buf
	}
	binary.LittleEndian.PutUint16(buf[0:2], sockAddrFamilyINet6)
	copy(buf[8:24], v6)
	return buf
}
