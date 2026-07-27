// Package hostinfo resolves the identity and network facts a discovery
// advertiser needs: the server's advertised name, the multicast-capable
// interfaces, and the host's IP addresses. It centralizes the ad-hoc
// os.Hostname()/net.Interfaces() logic so the mDNS and WS-Discovery responders
// agree on what to advertise.
package hostinfo

import (
	"net"
	"os"
	"strings"
)

// FallbackName is advertised when the OS hostname cannot be determined. It
// matches the standalone SMB machine-name fallback used elsewhere in the tree.
const FallbackName = "DITTOFS"

// ServerName returns the name to advertise for this host: the first label of the
// OS hostname, upper-cased (NetBIOS convention), or FallbackName when the
// hostname is empty/unavailable. e.g. "vm2.cubbit.local" -> "VM2".
func ServerName() string {
	return serverNameFrom(os.Hostname())
}

// serverNameFrom is the pure core of ServerName, taking the os.Hostname() result
// so it can be tested without depending on the real hostname.
func serverNameFrom(h string, err error) string {
	if err != nil || h == "" {
		return FallbackName
	}
	if i := strings.IndexByte(h, '.'); i >= 0 {
		h = h[:i]
	}
	h = strings.TrimSpace(h)
	if h == "" {
		return FallbackName
	}
	return strings.ToUpper(h)
}

// DefaultNamePrefix is the product-branded prefix of the auto-generated
// discovery name.
const DefaultNamePrefix = "DittoFS"

// DefaultDiscoveryName is the instance-wide name advertised on the LAN when the
// operator has not set an override: "DittoFS-<hostname>" (e.g. "DittoFS-VM2"),
// so multiple DittoFS servers on one network stay distinguishable by default.
// Falls back to the bare prefix when the hostname is unavailable.
func DefaultDiscoveryName() string {
	name := ServerName()
	if name == FallbackName {
		return DefaultNamePrefix
	}
	return DefaultNamePrefix + "-" + name
}

// NetBIOSSafe folds an arbitrary discovery name into a NetBIOS-legal computer
// name for WS-Discovery: upper-cased, illegal characters replaced with '-',
// trimmed of leading/trailing '-', and capped at the 15-character NetBIOS limit.
// Returns FallbackName if nothing legal survives. mDNS instance names have no
// such constraint and use the raw name.
func NetBIOSSafe(name string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(name) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > 15 {
		s = strings.TrimRight(s[:15], "-")
	}
	if s == "" {
		return FallbackName
	}
	return s
}

// MulticastInterfaces returns the interfaces suitable for multicast discovery:
// up, multicast-capable, not loopback, not point-to-point, and carrying an IPv4
// address. Empty when none qualify.
//
// The last two conditions are what keep the responders quick. Both of them
// announce by walking this list and sending once per interface, serialized
// because the multicast egress interface is per-socket state, and each send is
// capped by a write deadline rather than allowed to block forever. So every
// interface that cannot carry the traffic still costs the full deadline before
// the walk moves on. A host with VPN tunnels up shows this plainly: a Mac with
// 23 "up, multicast, non-loopback" interfaces — six utun tunnels, awdl0, llw0,
// the anpi/bridge/ap set — spent 4.5 seconds inside a single announce, which
// delays the listener that starts after it and the shutdown that follows it.
//
// A point-to-point link is a tunnel with no LAN segment to be discovered on.
// An interface with no IPv4 address cannot source these datagrams at all: both
// responders speak IPv4 only (WS-Discovery advertises IPv4 XAddrs, mDNS binds
// udp4 and joins the IPv4 group). Neither is a judgement call about which
// networks matter — they are interfaces the packet provably cannot go out on.
func MulticastInterfaces() []net.Interface {
	all, err := net.Interfaces()
	if err != nil {
		return nil
	}
	out := make([]net.Interface, 0, len(all))
	for _, ifi := range all {
		// Flags first: they are already in hand, while Addrs is a per-interface
		// syscall not worth spending on an interface the flags rule out.
		if !eligibleFlags(ifi.Flags) {
			continue
		}
		addrs, aerr := ifi.Addrs()
		if aerr != nil {
			continue
		}
		if hasIPv4(addrs) {
			out = append(out, ifi)
		}
	}
	return out
}

// eligibleFlags reports whether an interface's flags allow discovery traffic.
func eligibleFlags(flags net.Flags) bool {
	const required = net.FlagUp | net.FlagMulticast
	if flags&required != required {
		return false
	}
	return flags&(net.FlagLoopback|net.FlagPointToPoint) == 0
}

// hasIPv4 reports whether any of the addresses is IPv4.
func hasIPv4(addrs []net.Addr) bool {
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
			return true
		}
	}
	return false
}

// AllHostIPs returns the host's non-loopback, non-link-local unicast IPs across
// all interfaces, for A/AAAA records and WS-Discovery XAddrs.
func AllHostIPs() []net.IP {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []net.IP
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipNet.IP
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			continue
		}
		out = append(out, ip)
	}
	return out
}

// PrimaryIPv4 returns the host's first usable IPv4 address, or nil when the host
// has no routable IPv4. Used as the WS-Discovery XAddrs host.
func PrimaryIPv4() net.IP {
	for _, ip := range AllHostIPs() {
		if v4 := ip.To4(); v4 != nil {
			return v4
		}
	}
	return nil
}
