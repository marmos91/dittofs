package fixture

import (
	"net"
	"runtime"
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

// The network is the one the fixture's server is on: the host's, or, for dt
// in a dtc container on a bridge network, that container's.
func TestNetwork(t *testing.T) {
	for _, c := range []struct{ in, hostnet, want string }{
		{"", "", "host"},
		{"1", "1", "host"},
		{"1", "", "container:dtc-1"},
	} {
		t.Setenv("DT_IN_CONTAINER", c.in)
		t.Setenv("DT_HOSTNET", c.hostnet)
		t.Setenv("DT_CONTAINER_NAME", "dtc-1")
		if got := Network(); got != c.want {
			t.Errorf("in %q, hostnet %q: %s, want %s", c.in, c.hostnet, got, c.want)
		}
	}
}

// What dt sets up in fixtures, and what it doesn't yet.
func TestUnsupported(t *testing.T) {
	mountsHere, kerberosHere := "only on Linux", "Kerberos only for SMB on Linux"
	if runtime.GOOS == "linux" {
		mountsHere, kerberosHere = "", ""
	}
	for _, tc := range []struct {
		f    registry.Fixture
		want string
	}{
		{registry.Fixture{Profile: "memory", NFS: registry.NFS{Version: "export"}, Squash: "none", Users: []string{"1"}, Lease: "30s"}, ""},
		{registry.Fixture{Profile: "{profile}", NFS: registry.NFS{Version: "{nfs}"}}, ""},
		{registry.Fixture{Profile: "badger-s3"}, ""},
		{registry.Fixture{Profile: "postgres-s3"}, `the "postgres-s3" profile`},
		{registry.Fixture{Server: "knfsd", NFS: registry.NFS{Version: "export"}, Squash: "none", Lease: "30s"}, ""},
		{registry.Fixture{Profile: "memory", Server: "ganesha"}, "ganesha fixture"},
		{registry.Fixture{Profile: "memory", NFS: registry.NFS{Version: "4.1"}}, mountsHere},
		{registry.Fixture{Profile: "memory", NFS: registry.NFS{Version: "4.2"}}, "mount NFS 4.2"},
		{registry.Fixture{Profile: "memory", SMB: registry.SMB{Enabled: true, Encryption: "preferred", Port: 445}, Shares: "s.yaml", Reset: "each"}, ""},
		{registry.Fixture{Profile: "memory", SMB: registry.SMB{Enabled: true, Encryption: "{enc}"}, Reset: "{reset}"}, ""},
		{registry.Fixture{Profile: "memory", SMB: registry.SMB{Enabled: true, Encryption: "required"}}, `SMB encryption "required"`},
		{registry.Fixture{Profile: "memory", SMB: registry.SMB{Enabled: true, Mount: true}}, "mount SMB"},
		{registry.Fixture{Profile: "memory", SMB: registry.SMB{Enabled: true}, Kerberos: true}, kerberosHere},
		{registry.Fixture{Profile: "memory", Kerberos: true}, "Kerberos only for SMB"},
		{registry.Fixture{Profile: "memory", Reset: "once"}, `reset shares "once"`},
	} {
		if got := Unsupported(&tc.f); (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%+v: %q, want %q", tc.f, got, tc.want)
		}
	}
}
