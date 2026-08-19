package shares

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/local/fs"
	metamem "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// The dirty_expire_seconds knob decides how long an acknowledged write may stay
// non-durable, so every branch of its parse is worth pinning: a value silently
// read as zero would hand the share the default instead of what the operator
// asked for, and a value read as negative would switch the loop off entirely.
func TestDirtyExpiryFromConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  map[string]any
		want time.Duration
	}{
		{"absent takes the journal default", map[string]any{}, 0},
		{"plain value", map[string]any{"dirty_expire_seconds": float64(5)}, 5 * time.Second},
		{"fractional value above the floor", map[string]any{"dirty_expire_seconds": 2.5}, 2500 * time.Millisecond},
		{"sub-second clamps to the floor", map[string]any{"dirty_expire_seconds": 0.001}, minDirtyExpire},
		{"just under the floor clamps", map[string]any{"dirty_expire_seconds": 0.999}, minDirtyExpire},
		{"at the floor is kept", map[string]any{"dirty_expire_seconds": float64(1)}, time.Second},
		{"negative disables", map[string]any{"dirty_expire_seconds": float64(-1)}, -time.Second},
		{"explicit zero takes the default", map[string]any{"dirty_expire_seconds": float64(0)}, 0},
		{"non-numeric ignored", map[string]any{"dirty_expire_seconds": "30s"}, 0},
		{"bool ignored", map[string]any{"dirty_expire_seconds": true}, 0},
		{"NaN ignored", map[string]any{"dirty_expire_seconds": math.NaN()}, 0},
		{"+Inf ignored", map[string]any{"dirty_expire_seconds": math.Inf(1)}, 0},
		{"-Inf ignored", map[string]any{"dirty_expire_seconds": math.Inf(-1)}, 0},
		{"overflows a Duration, ignored", map[string]any{"dirty_expire_seconds": 1e18}, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := dirtyExpiryFromConfig(tc.cfg); got != tc.want {
				t.Fatalf("dirtyExpiryFromConfig = %v, want %v", got, tc.want)
			}
		})
	}
}

// The parse is only half the knob: the value has to survive the trip through
// FSStoreOptions into the journal's config, and nothing observable would change
// if that assignment were dropped. This drives the whole path from the config
// map an operator edits down to the fsync, using only exported API: a write
// that never asks for durability must reach the durable watermark on its own
// once the configured interval elapses.
func TestDirtyExpiryFromConfig_ReachesTheJournal(t *testing.T) {
	ctx := context.Background()
	cfg := &fakeBlockStoreConfig{cfg: map[string]any{
		"path":                 t.TempDir(),
		"dirty_expire_seconds": float64(1),
	}}
	mds := metamem.NewMemoryMetadataStoreWithDefaults()
	t.Cleanup(func() { _ = mds.Close() })

	store, err := CreateLocalStoreFromConfig(ctx, "fs", cfg, "dirty-expire", nil, mds, false)
	if err != nil {
		t.Fatalf("CreateLocalStoreFromConfig: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	fsStore := store.(*fs.FSStore)

	const payloadID = "unfsynced"
	payload := []byte("never committed by the client")
	if err := fsStore.WriteAt(ctx, payloadID, 0, payload); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if n, _ := fsStore.DurableExtent(ctx, payloadID); n != 0 {
		t.Fatalf("durable extent = %d before any commit, want 0", n)
	}

	// No Commit call anywhere: only the configured dirty-age loop can move this.
	deadline := time.Now().Add(20 * time.Second)
	for {
		n, _ := fsStore.DurableExtent(ctx, payloadID)
		if n >= int64(len(payload)) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("durable extent stuck at %d after the dirty-age interval; "+
				"dirty_expire_seconds did not reach the journal", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
