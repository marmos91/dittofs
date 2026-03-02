package kerberos

import (
	"sync"
	"testing"
	"time"
)

func TestReplayCache_FirstSeenNotReplay(t *testing.T) {
	rc := NewReplayCache(5 * time.Minute)

	now := time.Now()
	isReplay := rc.Check("alice@EXAMPLE.COM", now, 123, "nfs/server.example.com")
	if isReplay {
		t.Fatal("expected first-seen authenticator to not be a replay")
	}
}

func TestReplayCache_DuplicateDetected(t *testing.T) {
	rc := NewReplayCache(5 * time.Minute)

	now := time.Now()
	// First check: not a replay
	isReplay := rc.Check("alice@EXAMPLE.COM", now, 123, "nfs/server.example.com")
	if isReplay {
		t.Fatal("expected first-seen to not be a replay")
	}

	// Second check with same parameters: replay detected
	isReplay = rc.Check("alice@EXAMPLE.COM", now, 123, "nfs/server.example.com")
	if !isReplay {
		t.Fatal("expected duplicate authenticator to be detected as replay")
	}
}

func TestReplayCache_DifferentTupleFieldNotReplay(t *testing.T) {
	now := time.Now()
	base := struct {
		principal string
		cusec     int
		service   string
	}{"alice@EXAMPLE.COM", 123, "nfs/server.example.com"}

	tests := []struct {
		name      string
		principal string
		cusec     int
		service   string
	}{
		{"DifferentPrincipal", "bob@EXAMPLE.COM", base.cusec, base.service},
		{"DifferentCusec", base.principal, 456, base.service},
		{"DifferentService", base.principal, base.cusec, "cifs/server.example.com"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rc := NewReplayCache(5 * time.Minute)
			rc.Check(base.principal, now, base.cusec, base.service)

			if rc.Check(tt.principal, now, tt.cusec, tt.service) {
				t.Fatalf("expected different %s to not be a replay", tt.name)
			}
		})
	}
}

func TestReplayCache_ExpiredEntryNotReplay(t *testing.T) {
	// Very short TTL for testing
	rc := NewReplayCache(50 * time.Millisecond)

	now := time.Now()
	rc.Check("alice@EXAMPLE.COM", now, 123, "nfs/server.example.com")

	// Wait for expiry
	time.Sleep(100 * time.Millisecond)

	// Should not be detected as replay after expiry
	isReplay := rc.Check("alice@EXAMPLE.COM", now, 123, "nfs/server.example.com")
	if isReplay {
		t.Fatal("expected expired entry to not be detected as replay")
	}
}

func TestReplayCache_ConcurrentAccess(t *testing.T) {
	rc := NewReplayCache(5 * time.Minute)
	now := time.Now()

	var wg sync.WaitGroup
	results := make([]bool, 100)

	// Launch 100 goroutines all checking the same authenticator
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx] = rc.Check("alice@EXAMPLE.COM", now, 999, "nfs/server.example.com")
		}(i)
	}
	wg.Wait()

	// Exactly one should have been "not a replay" (false), the rest should be replays (true)
	falseCount := 0
	for _, r := range results {
		if !r {
			falseCount++
		}
	}
	if falseCount != 1 {
		t.Fatalf("expected exactly 1 non-replay result, got %d", falseCount)
	}
}

func TestReplayCache_DefaultTTL(t *testing.T) {
	rc := NewReplayCache(0) // Should use default
	if rc.ttl != DefaultReplayCacheTTL {
		t.Fatalf("expected default TTL %v, got %v", DefaultReplayCacheTTL, rc.ttl)
	}
}
