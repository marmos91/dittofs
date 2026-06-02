//go:build integration

package s3

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"lukechampine.com/blake3"
)

// ds3ProbeConfig builds an s3.Config from the DS3_* env vars used by the
// live validation harness (see /tmp/ds3.env). Skips when unset.
func ds3ProbeConfig(t *testing.T) Config {
	t.Helper()
	endpoint := os.Getenv("DS3_ENDPOINT")
	if endpoint == "" {
		t.Skip("DS3_ENDPOINT not set; skipping DS3 consistency probe")
	}
	if !strings.HasPrefix(endpoint, "http") {
		endpoint = "https://" + endpoint
	}
	region := os.Getenv("DS3_REGION")
	if region == "" {
		region = "eu-central-1"
	}
	return Config{
		Bucket:    os.Getenv("DS3_BUCKET"),
		Region:    region,
		Endpoint:  endpoint,
		AccessKey: os.Getenv("DS3_ACCESS_KEY"),
		SecretKey: os.Getenv("DS3_SECRET_KEY"),
		KeyPrefix: "probe/" + t.Name() + "/",
	}
}

// TestDS3_ReadAfterWriteConsistency measures the read-after-write window:
// PUT a fresh random block, then HEAD it in a tight loop, recording how
// many HEAD attempts (not wall time — the round-trip itself is tens of ms)
// elapse before the object becomes visible. attempts==1 on every iteration
// means DS3 is strongly read-after-write consistent for a single
// PUT→HEAD, which rules eventual consistency OUT as the cause of the
// snapshot "verify chunk not found" failure (that must instead be a hash
// that was never uploaded). attempts>1 would implicate a propagation
// window the snapshot verify must tolerate with a HEAD retry/backoff.
func TestDS3_ReadAfterWriteConsistency(t *testing.T) {
	cfg := ds3ProbeConfig(t)
	store, err := NewFromConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	defer func() { _ = store.Close() }()

	const iterations = 12
	var maxLag time.Duration
	lagged := 0 // iterations needing >1 HEAD attempt (true propagation lag)

	for i := 0; i < iterations; i++ {
		data := make([]byte, 64*1024)
		if _, err := rand.Read(data); err != nil {
			t.Fatalf("rand: %v", err)
		}
		hash := block.ContentHash(blake3.Sum256(data))
		ctx := context.Background()

		putStart := time.Now()
		if err := store.Put(ctx, hash, data); err != nil {
			t.Fatalf("iter %d: Put: %v", i, err)
		}
		putDur := time.Since(putStart)

		// Tight HEAD loop, measuring time-to-visible.
		deadline := time.Now().Add(15 * time.Second)
		var lag time.Duration
		visStart := time.Now()
		attempts := 0
		for {
			attempts++
			_, herr := store.Head(ctx, hash)
			if herr == nil {
				lag = time.Since(visStart)
				break
			}
			if !errors.Is(herr, block.ErrChunkNotFound) {
				t.Fatalf("iter %d: Head non-404 error: %v", i, herr)
			}
			if time.Now().After(deadline) {
				t.Errorf("iter %d: hash %s NEVER became visible within 15s (%d HEAD attempts) — durability gap, not lag", i, hash, attempts)
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if attempts > 1 {
			lagged++
			if lag > maxLag {
				maxLag = lag
			}
		}
		t.Logf("iter %2d: put=%6s  head-attempts=%2d  head-rtt=%8s", i, putDur.Round(time.Millisecond), attempts, lag.Round(time.Millisecond))
		_ = store.Delete(ctx, hash)
	}

	t.Logf("SUMMARY: %d/%d iterations needed >1 HEAD attempt (true propagation lag); max settle=%s", lagged, iterations, maxLag.Round(time.Millisecond))
	if lagged > 0 {
		fmt.Printf("DS3 SHOWS PROPAGATION LAG: %d/%d PUTs not visible on first HEAD (max settle %s) — snapshot verify needs HEAD retry/backoff\n", lagged, iterations, maxLag.Round(time.Millisecond))
	} else {
		fmt.Printf("DS3 strongly consistent: all %d PUTs visible on first HEAD — 'verify chunk not found' is a never-uploaded hash, not a lag\n", iterations)
	}
}

// TestDS3_PutThenGetBytes confirms a PUT is byte-faithful once visible, and
// that GET returns the exact bytes (rules out silent truncation/corruption
// on the DS3 path independent of the consistency window).
func TestDS3_PutThenGetBytes(t *testing.T) {
	cfg := ds3ProbeConfig(t)
	store, err := NewFromConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	defer func() { _ = store.Close() }()

	ctx := context.Background()
	data := make([]byte, 256*1024+777) // non-block-aligned
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	hash := block.ContentHash(blake3.Sum256(data))
	if err := store.Put(ctx, hash, data); err != nil {
		t.Fatalf("Put: %v", err)
	}
	defer func() { _ = store.Delete(ctx, hash) }()

	// Poll until visible (consistency window), then byte-compare.
	deadline := time.Now().Add(15 * time.Second)
	for {
		got, gerr := store.Get(ctx, hash)
		if gerr == nil {
			if !bytes.Equal(got, data) {
				t.Fatalf("GET bytes differ: got %d bytes, want %d", len(got), len(data))
			}
			return
		}
		if !errors.Is(gerr, block.ErrChunkNotFound) {
			t.Fatalf("Get error: %v", gerr)
		}
		if time.Now().After(deadline) {
			t.Fatalf("hash never became visible for GET within 15s")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
