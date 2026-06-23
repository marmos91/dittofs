package handlers

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/nsm/types"
)

// TestCallbackHostFromSource exercises the SM_MON callback-host derivation
// directly. The callback target is taken from the request's transport source,
// not the client-supplied my_name: any routable source IP (including loopback,
// for same-host clients) is accepted, while link-local sources (notably the
// cloud IMDS address 169.254.169.254) and non-IP inputs are rejected.
func TestCallbackHostFromSource(t *testing.T) {
	cases := []struct {
		addr     string
		wantHost string
		wantErr  bool
		desc     string
	}{
		{"10.0.0.1:600", "10.0.0.1", false, "RFC-1918 source — allowed"},
		{"203.0.113.5:711", "203.0.113.5", false, "public source — allowed"},
		{"127.0.0.1:829", "127.0.0.1", false, "loopback source — allowed (same-host client)"},
		{"[::1]:829", "::1", false, "IPv6 loopback source — allowed"},
		{"10.0.0.1", "10.0.0.1", false, "bare host without port — allowed"},
		{"169.254.169.254:600", "", true, "cloud IMDS source — rejected (link-local)"},
		{"169.254.0.1:600", "", true, "link-local source — rejected"},
		{"[fe80::1]:600", "", true, "IPv6 link-local source — rejected"},
		{"not-an-ip:600", "", true, "non-IP host — rejected"},
		{"", "", true, "empty — rejected"},
	}
	for _, tc := range cases {
		host, err := callbackHostFromSource(tc.addr)
		if tc.wantErr {
			if err == nil {
				t.Errorf("[%s] addr=%q: expected error, got host=%q", tc.desc, tc.addr, host)
			}
			continue
		}
		if err != nil {
			t.Errorf("[%s] addr=%q: unexpected error: %v", tc.desc, tc.addr, err)
		}
		if host != tc.wantHost {
			t.Errorf("[%s] addr=%q: host=%q, want %q", tc.desc, tc.addr, host, tc.wantHost)
		}
	}
}

// TestMon_CallbackFromSource verifies the SM_MON handler keys the SM_NOTIFY
// callback off the request's transport source, not the client-supplied my_name.
// Real statd implementations (Linux, macOS) send my_name as a hostname such as
// "localhost"; that must NOT cause a rejection. A same-host (loopback) client
// must be accepted — this is the macOS-locking-against-localhost case that
// previously failed STAT_FAIL ("my_name is not an IP literal"). Only link-local
// sources (cloud IMDS) are rejected.
func TestMon_CallbackFromSource(t *testing.T) {
	cases := []struct {
		name       string
		clientAddr string
		wantFail   bool
	}{
		{"loopback source accepted (macOS localhost)", "127.0.0.1:829", false},
		{"IPv6 loopback source accepted", "[::1]:829", false},
		{"private source accepted", "10.0.0.5:600", false},
		{"public source accepted", "203.0.113.1:600", false},
		{"IMDS source rejected", "169.254.169.254:600", true},
		{"IPv6 link-local source rejected", "[fe80::1]:600", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHandler(t, &fakeDispatcher{})
			// my_name is a hostname, exactly as Linux/macOS statd send it.
			mon := &types.Mon{
				MonID: types.MonID{
					MonName: "peer.example",
					MyID: types.MyID{
						MyName: "localhost",
						MyProg: 100021, MyVers: 4, MyProc: 23,
					},
				},
			}
			data := encodeMon(t, mon)
			ctx := &NSMHandlerContext{Context: context.Background(), ClientAddr: tc.clientAddr}
			result, err := h.Mon(ctx, data)
			if err != nil {
				t.Fatalf("Mon must not return a Go error: %v", err)
			}
			gotFail := result.NSMStatus == types.StatFail
			if tc.wantFail && !gotFail {
				t.Errorf("source %q should have been rejected (STAT_FAIL), got STAT_SUCC", tc.clientAddr)
			}
			if !tc.wantFail && gotFail {
				t.Errorf("source %q should have been accepted (STAT_SUCC), got STAT_FAIL", tc.clientAddr)
			}
		})
	}
}
