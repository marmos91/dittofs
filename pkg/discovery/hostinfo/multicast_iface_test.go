package hostinfo

import (
	"net"
	"testing"
)

func v4(cidr string) []net.Addr {
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		panic(err)
	}
	return []net.Addr{n}
}

// TestMulticastEligible pins which interfaces the discovery responders announce
// on. Each excluded shape is one that costs a full multicast write deadline
// before the send fails, and a laptop has enough of them to add seconds to
// startup and shutdown.
func TestMulticastEligible(t *testing.T) {
	const up = net.FlagUp | net.FlagMulticast | net.FlagRunning

	cases := []struct {
		name  string
		flags net.Flags
		addrs []net.Addr
		want  bool
	}{
		{"ordinary LAN interface", up | net.FlagBroadcast, v4("192.168.1.20/24"), true},
		{"IPv4 link-local still counts", up | net.FlagBroadcast, v4("169.254.3.4/16"), true},
		{"down", net.FlagMulticast, v4("192.168.1.20/24"), false},
		{"no multicast", net.FlagUp | net.FlagRunning, v4("192.168.1.20/24"), false},
		{"loopback", up | net.FlagLoopback, v4("127.0.0.1/8"), false},
		{
			// utun0 and friends: a tunnel has no LAN segment to be found on.
			name:  "point-to-point tunnel",
			flags: up | net.FlagPointToPoint,
			addrs: v4("10.8.0.2/32"),
			want:  false,
		},
		{
			// awdl0 / llw0 / anpi*: up and multicast-capable, but no IPv4 to
			// source an IPv4 datagram from.
			name:  "no IPv4 address",
			flags: up | net.FlagBroadcast,
			addrs: []net.Addr{&net.IPNet{IP: net.ParseIP("fe80::1"), Mask: net.CIDRMask(64, 128)}},
			want:  false,
		},
		{"no addresses at all", up | net.FlagBroadcast, nil, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Mirrors the order MulticastInterfaces applies them: flags gate the
			// address lookup, so an ineligible flag set is excluded regardless
			// of what addresses the interface carries.
			got := eligibleFlags(tc.flags) && hasIPv4(tc.addrs)
			if got != tc.want {
				t.Errorf("eligible(%v, %v) = %v, want %v", tc.flags, tc.addrs, got, tc.want)
			}
		})
	}
}
