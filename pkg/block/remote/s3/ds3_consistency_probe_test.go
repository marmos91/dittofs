//go:build integration

package s3

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
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

// probeBlock returns random content plus a block ID no earlier iteration
// can have written, so each PUT is a first write rather than an overwrite.
func probeBlock(t *testing.T, size int) (blockID string, data []byte) {
	t.Helper()
	data = make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return "probe-" + hex.EncodeToString(data[:16]), data
}

// TestDS3_ReadAfterWriteConsistency measures the read-after-write window:
// PUT a fresh random block, then probe it in a tight loop, recording how
// many attempts (not wall time — the round-trip itself is tens of ms)
// elapse before the object becomes visible. The probe is a one-byte range
// GET, the cheapest visibility check the block-keyed contract offers.
// attempts==1 on every iteration means DS3 is strongly read-after-write
// consistent for a single PUT→GET, which rules eventual consistency OUT as
// the cause of the snapshot "verify chunk not found" failure (that must
// instead be a block that was never uploaded). attempts>1 would implicate a
// propagation window the snapshot verify must tolerate with retry/backoff.
func TestDS3_ReadAfterWriteConsistency(t *testing.T) {
	cfg := ds3ProbeConfig(t)
	store, err := NewFromConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	defer func() { _ = store.Close() }()

	const iterations = 12
	var maxLag time.Duration
	lagged := 0 // iterations needing >1 probe (true propagation lag)

	for i := 0; i < iterations; i++ {
		blockID, data := probeBlock(t, 64*1024)
		ctx := context.Background()

		putStart := time.Now()
		if err := store.PutBlock(ctx, blockID, bytes.NewReader(data)); err != nil {
			t.Fatalf("iter %d: PutBlock: %v", i, err)
		}
		putDur := time.Since(putStart)

		// Tight probe loop, measuring time-to-visible.
		deadline := time.Now().Add(15 * time.Second)
		var lag time.Duration
		visStart := time.Now()
		attempts := 0
		for {
			attempts++
			_, gerr := store.GetBlockRange(ctx, blockID, 0, 1)
			if gerr == nil {
				lag = time.Since(visStart)
				break
			}
			if !errors.Is(gerr, block.ErrChunkNotFound) {
				t.Fatalf("iter %d: probe non-404 error: %v", i, gerr)
			}
			if time.Now().After(deadline) {
				t.Errorf("iter %d: block %s NEVER became visible within 15s (%d probes) — durability gap, not lag", i, blockID, attempts)
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
		t.Logf("iter %2d: put=%6s  probes=%2d  probe-rtt=%8s", i, putDur.Round(time.Millisecond), attempts, lag.Round(time.Millisecond))
		_ = store.DeleteBlock(ctx, blockID)
	}

	t.Logf("SUMMARY: %d/%d iterations needed >1 probe (true propagation lag); max settle=%s", lagged, iterations, maxLag.Round(time.Millisecond))
	if lagged > 0 {
		fmt.Printf("DS3 SHOWS PROPAGATION LAG: %d/%d PUTs not visible on first probe (max settle %s) — snapshot verify needs retry/backoff\n", lagged, iterations, maxLag.Round(time.Millisecond))
	} else {
		fmt.Printf("DS3 strongly consistent: all %d PUTs visible on first probe — 'verify chunk not found' is a never-uploaded block, not a lag\n", iterations)
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
	blockID, data := probeBlock(t, 256*1024+777) // non-block-aligned
	if err := store.PutBlock(ctx, blockID, bytes.NewReader(data)); err != nil {
		t.Fatalf("PutBlock: %v", err)
	}
	defer func() { _ = store.DeleteBlock(ctx, blockID) }()

	// Poll until visible (consistency window), then byte-compare.
	deadline := time.Now().Add(15 * time.Second)
	for {
		got, gerr := store.GetBlock(ctx, blockID)
		if gerr == nil {
			if !bytes.Equal(got, data) {
				t.Fatalf("GET bytes differ: got %d bytes, want %d", len(got), len(data))
			}
			return
		}
		if !errors.Is(gerr, block.ErrChunkNotFound) {
			t.Fatalf("GetBlock error: %v", gerr)
		}
		if time.Now().After(deadline) {
			t.Fatalf("block never became visible for GET within 15s")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
