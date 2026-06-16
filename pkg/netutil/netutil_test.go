package netutil

import "testing"

// TestIsNonLoopbackHost is a table test for the host classifier that gates the
// cleartext-credentials / unencrypted-traffic startup warnings.
func TestIsNonLoopbackHost(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"", true},           // empty = wildcard bind (":port", all interfaces)
		{"127.0.0.1", false}, // explicit loopback
		{"127.0.0.5", false}, // anything in 127.0.0.0/8 is loopback
		{"::1", false},
		{"[::1]", false},
		{"localhost", false},
		{"0.0.0.0", true}, // wildcard reaches off-host
		{"::", true},
		{"[::]", true},
		{"10.0.0.5", true},        // private but off-host
		{"192.168.1.20", true},    // off-host
		{"api.example.com", true}, // hostname → assume off-host
	}
	for _, c := range cases {
		if got := IsNonLoopbackHost(c.host); got != c.want {
			t.Errorf("IsNonLoopbackHost(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}
