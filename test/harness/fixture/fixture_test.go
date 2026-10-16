package fixture

import (
	"net"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/test/harness/registry"
)

// {server} is the first IPv4 address of an interface that is up and not
// loopback, leaving out link-local addresses.
func TestAddress(t *testing.T) {
	ip := func(s string) net.Addr { return &net.IPNet{IP: net.ParseIP(s), Mask: net.CIDRMask(24, 32)} }
	up := net.FlagUp
	for _, tc := range []struct {
		name   string
		ifaces []iface
		want   string
	}{
		{"loopback only", []iface{{up | net.FlagLoopback, []net.Addr{ip("127.0.0.1")}}}, ""},
		{"down", []iface{{0, []net.Addr{ip("10.0.0.2")}}}, ""},
		{"link-local and IPv6 skipped", []iface{{up, []net.Addr{ip("169.254.1.1"), ip("fe80::1"), ip("2001:db8::1")}}}, ""},
		{"first that qualifies", []iface{
			{up | net.FlagLoopback, []net.Addr{ip("127.0.0.1")}},
			{0, []net.Addr{ip("10.0.0.2")}},
			{up, []net.Addr{ip("fe80::1"), ip("192.168.1.20")}},
			{up, []net.Addr{ip("10.1.0.4")}},
		}, "192.168.1.20"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			interfaces = func() ([]iface, error) { return tc.ifaces, nil }
			t.Cleanup(func() { interfaces = realInterfaces })
			got, err := address()
			if got != tc.want || (err == nil) != (tc.want != "") {
				t.Errorf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

var realInterfaces = interfaces

// What dt sets up in fixtures, and what it doesn't yet.
func TestUnsupported(t *testing.T) {
	for _, tc := range []struct {
		f    registry.Fixture
		want string
	}{
		{registry.Fixture{Profile: "memory", NFS: registry.NFS{Version: "export"}, Squash: "none", Users: []int{1}, Lease: "30s"}, ""},
		{registry.Fixture{Profile: "{profile}", NFS: registry.NFS{Version: "{nfs}"}}, ""},
		{registry.Fixture{Profile: "badger-s3"}, ""},
		{registry.Fixture{Profile: "postgres-s3"}, `the "postgres-s3" profile`},
		{registry.Fixture{Profile: "memory", Server: "knfsd"}, "knfsd fixture"},
		{registry.Fixture{Profile: "memory", NFS: registry.NFS{Version: "4.1"}}, "mount NFS"},
		{registry.Fixture{Profile: "memory", SMB: registry.SMB{Enabled: true}}, "SMB"},
		{registry.Fixture{Profile: "memory", Shares: "s.yaml"}, "shares file"},
		{registry.Fixture{Profile: "memory", Kerberos: true}, "Kerberos"},
		{registry.Fixture{Profile: "memory", Reset: "each"}, "reset"},
	} {
		if got := Unsupported(&tc.f); (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%+v: %q, want %q", tc.f, got, tc.want)
		}
	}
}
