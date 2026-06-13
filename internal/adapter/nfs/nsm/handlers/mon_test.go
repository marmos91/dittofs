package handlers

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/nsm/types"
)

// TestValidateCallbackName exercises the SM_MON callback-address SSRF guard
// directly: only safe IP literals are accepted; loopback, link-local (including
// the cloud IMDS address 169.254.169.254), and non-IP hostnames are rejected.
func TestValidateCallbackName(t *testing.T) {
	cases := []struct {
		name    string
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
		err := validateCallbackName(tc.name)
		if tc.wantErr && err == nil {
			t.Errorf("[%s] name=%q: expected error, got nil", tc.desc, tc.name)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("[%s] name=%q: unexpected error: %v", tc.desc, tc.name, err)
		}
	}
}

// TestMon_CallbackNameSSRF_Rejected verifies the SM_MON handler rejects unsafe
// callback addresses with STAT_FAIL (no Go error) and accepts safe IP literals.
func TestMon_CallbackNameSSRF_Rejected(t *testing.T) {
	cases := []struct {
		name         string
		callbackHost string
		wantFail     bool
	}{
		{"loopback rejected", "127.0.0.1", true},
		{"link-local IMDS", "169.254.169.254", true},
		{"IPv6 loopback", "::1", true},
		{"fe80 link-local", "fe80::1", true},
		{"hostname rejected", "callback.example", true},
		{"valid IP accepted", "10.0.0.5", false},
		{"valid public IP", "203.0.113.1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHandler(t, &fakeDispatcher{})
			mon := &types.Mon{
				MonID: types.MonID{
					MonName: "peer.example",
					MyID: types.MyID{
						MyName: tc.callbackHost,
						MyProg: 100021, MyVers: 4, MyProc: 23,
					},
				},
			}
			data := encodeMon(t, mon)
			ctx := &NSMHandlerContext{Context: context.Background(), ClientAddr: "10.0.0.1:600"}
			result, err := h.Mon(ctx, data)
			if err != nil {
				t.Fatalf("Mon must not return a Go error: %v", err)
			}
			gotFail := result.NSMStatus == types.StatFail
			if tc.wantFail && !gotFail {
				t.Errorf("callback host %q should have been rejected (STAT_FAIL), got STAT_SUCC", tc.callbackHost)
			}
			if !tc.wantFail && gotFail {
				t.Errorf("callback host %q should have been accepted (STAT_SUCC), got STAT_FAIL", tc.callbackHost)
			}
		})
	}
}
