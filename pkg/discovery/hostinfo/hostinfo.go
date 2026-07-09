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

// MulticastInterfaces returns the interfaces suitable for multicast discovery:
// up, multicast-capable, and not loopback. Empty when none qualify.
func MulticastInterfaces() []net.Interface {
	all, err := net.Interfaces()
	if err != nil {
		return nil
	}
	out := make([]net.Interface, 0, len(all))
	for _, ifi := range all {
		if ifi.Flags&net.FlagUp == 0 {
			continue
		}
		if ifi.Flags&net.FlagMulticast == 0 {
			continue
		}
		if ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		out = append(out, ifi)
	}
	return out
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
