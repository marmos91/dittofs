package wsd

import (
	"context"
	"net"
	"testing"
	"time"
)

// TestResponder_StartStopNoDeadlock exercises the real socket path: Start must
// return promptly (it must not self-deadlock sending Hello while holding its
// lock) and Stop must tear everything down. It skips when the environment cannot
// bind the WS-Discovery UDP group or the metadata port.
func TestResponder_StartStopNoDeadlock(t *testing.T) {
	r := NewResponder("VM2", "CUBBIT", false, 1)

	done := make(chan error, 1)
	go func() { done <- r.Start(context.Background()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Skipf("cannot bind WS-Discovery sockets in this environment: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return within 5s — likely a lock self-deadlock")
	}

	// Stop must also complete promptly.
	stopped := make(chan error, 1)
	go func() { stopped <- r.Stop(context.Background()) }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return within 5s")
	}

	// Stop is idempotent.
	if err := r.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

func TestBuildXAddrs_OneURLPerIP(t *testing.T) {
	got := buildXAddrs("uuid-1", []net.IP{net.IPv4(192, 168, 1, 5), net.IPv4(10, 0, 0, 9)})
	want := "http://192.168.1.5:5357/uuid-1/ http://10.0.0.9:5357/uuid-1/"
	if got != want {
		t.Fatalf("buildXAddrs = %q, want %q", got, want)
	}
}

func TestPreferFirst_ReordersReachableFirst(t *testing.T) {
	ips := []net.IP{net.IPv4(172, 20, 63, 93), net.IPv4(192, 168, 100, 50)}
	got := preferFirst(ips, net.IPv4(192, 168, 100, 50))
	if !got[0].Equal(net.IPv4(192, 168, 100, 50)) {
		t.Fatalf("preferFirst did not put the reachable IP first: %v", got)
	}
	if len(got) != 2 || !got[1].Equal(net.IPv4(172, 20, 63, 93)) {
		t.Fatalf("preferFirst dropped/duplicated IPs: %v", got)
	}
}

func TestPreferFirst_UnknownIPLeavesOrderUnchanged(t *testing.T) {
	ips := []net.IP{net.IPv4(172, 20, 63, 93), net.IPv4(192, 168, 100, 50)}
	got := preferFirst(ips, net.IPv4(10, 0, 0, 1)) // not in the list
	if len(got) != 2 || !got[0].Equal(net.IPv4(172, 20, 63, 93)) {
		t.Fatalf("preferFirst must not inject a non-advertised IP: %v", got)
	}
}
