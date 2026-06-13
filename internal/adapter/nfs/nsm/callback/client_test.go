package callback

import "testing"

// TestValidateNSMCallbackHost verifies the dial-time SSRF guard: only IP
// literals that are neither loopback nor link-local may be dialled. The cloud
// IMDS address 169.254.169.254 falls under link-local and must be rejected.
func TestValidateNSMCallbackHost(t *testing.T) {
	cases := []struct {
		host    string
		wantErr bool
		desc    string
	}{
		{"10.0.0.1", false, "RFC-1918 private — allowed"},
		{"192.168.1.100", false, "RFC-1918 private — allowed"},
		{"203.0.113.5", false, "public IP — allowed"},
		{"::1", true, "IPv6 loopback — rejected"},
		{"127.0.0.1", true, "loopback — rejected"},
		{"127.1.2.3", true, "loopback subnet — rejected"},
		{"169.254.169.254", true, "cloud IMDS — rejected (link-local)"},
		{"169.254.0.1", true, "link-local — rejected"},
		{"fe80::1", true, "IPv6 link-local — rejected"},
		{"hostname.local", true, "hostname (not IP literal) — rejected"},
		{"", true, "empty — rejected"},
	}
	for _, tc := range cases {
		err := validateNSMCallbackHost(tc.host)
		if tc.wantErr && err == nil {
			t.Errorf("[%s] host=%q: expected error, got nil", tc.desc, tc.host)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("[%s] host=%q: unexpected error: %v", tc.desc, tc.host, err)
		}
	}
}

// TestValidateNSMCallbackHost_LoopbackEscapeHatch verifies the test-only
// override permits loopback so tests can bind a listener on 127.0.0.1.
func TestValidateNSMCallbackHost_LoopbackEscapeHatch(t *testing.T) {
	prev := allowNSMLoopbackCallback
	allowNSMLoopbackCallback = true
	defer func() { allowNSMLoopbackCallback = prev }()

	if err := validateNSMCallbackHost("127.0.0.1"); err != nil {
		t.Errorf("loopback should be allowed when allowNSMLoopbackCallback=true: %v", err)
	}
	// Link-local must still be rejected even with the loopback escape hatch.
	if err := validateNSMCallbackHost("169.254.169.254"); err == nil {
		t.Error("link-local must remain rejected regardless of loopback override")
	}
}
