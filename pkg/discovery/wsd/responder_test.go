package wsd

import (
	"context"
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
