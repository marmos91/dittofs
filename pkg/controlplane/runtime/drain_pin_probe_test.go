//go:build integration

package runtime

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/pkg/block/chunker"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/metadata"

	"github.com/marmos91/dittofs/pkg/metadata/store/badger/badgertest"
)

// The two constants the dfsbench cold barrier's worst-case bound is built from
// (internal/dfsbench/backend/dittofs.go): every unsynced straggler is assumed to
// sit alone in a segment of its own.
const (
	probeStragglerBytes = int64(chunker.MinChunkSize)
	probeSegmentBytes   = int64(256 << 20)
)

// probeRemoteConfig builds an s3 remote from PROBE_S3_* so the probe runs
// against whatever bucket the operator points it at. Skips when unset.
func probeRemoteConfig(t *testing.T, prefix string) *models.BlockStoreConfig {
	t.Helper()
	endpoint := os.Getenv("PROBE_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("PROBE_S3_ENDPOINT not set; skipping the drain-pin probe")
	}
	if !strings.HasPrefix(endpoint, "http") {
		endpoint = "https://" + endpoint
	}
	region := os.Getenv("PROBE_S3_REGION")
	if region == "" {
		region = "fr-par"
	}
	cfg := &models.BlockStoreConfig{Name: "probe-remote", Type: "s3"}
	if err := cfg.SetConfig(map[string]any{
		"bucket":            os.Getenv("PROBE_S3_BUCKET"),
		"region":            region,
		"endpoint":          endpoint,
		"prefix":            prefix,
		"access_key_id":     os.Getenv("PROBE_S3_ACCESS_KEY"),
		"secret_access_key": os.Getenv("PROBE_S3_SECRET_KEY"),
	}); err != nil {
		t.Fatalf("SetConfig(remote): %v", err)
	}
	return cfg
}

func probeEnvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// TestDrainPinProbe measures what #2822 asks for: per drain round, the unsynced
// residue against the local disk it ACTUALLY pins, versus the
// one-segment-per-straggler worst case the dfsbench cold barrier budgets for.
//
// It is a measurement, not an assertion — it fails only on a broken rig, and
// prints a table for a human to read. The numbers it needs are
// Stats.PinnedSegments/PinnedBytes, which count the sealed segments eviction's
// synced-gate refuses; that is the quantity the barrier estimates and never
// observes.
//
// Writes go through common.WriteToBlockStore/CommitBlockStore WITHOUT a forced
// DrainRollups, unlike byteVerifyFixture.writeFile: forcing the carve would
// manufacture the residue the probe is trying to observe.
func TestDrainPinProbe(t *testing.T) {
	prefix := fmt.Sprintf("drainpin/%d/", time.Now().UnixNano())
	remoteCfg := probeRemoteConfig(t, prefix)
	meta := badgertest.NewInMemory(t)
	fx := newByteVerifyFixtureOpts(t, meta, "memory", remoteCfg)
	defer fx.close()

	ctx := context.Background()
	const mib = 1 << 20

	files := probeEnvInt("PROBE_FILES", 24)
	fileMiB := probeEnvInt("PROBE_FILE_MIB", 48)
	rounds := probeEnvInt("PROBE_ROUNDS", 12)

	t.Logf("writing %d files x %d MiB = %d MiB to %s%s",
		files, fileMiB, files*fileMiB, os.Getenv("PROBE_S3_BUCKET"), prefix)

	start := time.Now()
	for i := 0; i < files; i++ {
		name := fmt.Sprintf("probe-%03d.bin", i)
		pid := fx.createEmptyFile(ctx, name)
		data := distinctBytes(fileMiB*mib, uint64(i)*0x9E3779B1)
		if err := common.WriteToBlockStore(ctx, fx.bs, pid, data, 0); err != nil {
			t.Fatalf("WriteToBlockStore %q: %v", name, err)
		}
		if err := common.CommitBlockStore(ctx, fx.bs, pid); err != nil {
			t.Fatalf("CommitBlockStore %q: %v", name, err)
		}
	}
	t.Logf("working set written in %s", time.Since(start).Round(time.Millisecond))

	probeRounds(t, fx, ctx, "bulk", rounds, true)

	// Phase 2 is the shape the cold barrier actually meets and the one #2822 is
	// about: a drained store plus a small uncarved tail. dfsbench writes its
	// working set, drains it, and then the barrier re-drains before each read
	// cell — so what it is looping against is a residue of leftovers, not the
	// whole working set. A few KiB appended to each file is the same thing
	// #2790's probe produced with 9 uncarved bytes.
	t.Log("--- phase 2: small uncarved tail on a drained store ---")
	for i := 0; i < files; i++ {
		name := fmt.Sprintf("probe-%03d.bin", i)
		pid := metadata.PayloadID(metadata.BuildPayloadID(fx.shareName, "/"+name))
		tail := distinctBytes(4096, uint64(i)*0xC2B2AE35)
		if err := common.WriteToBlockStore(ctx, fx.bs, pid, tail, uint64(fileMiB*mib)); err != nil {
			t.Fatalf("tail write %q: %v", name, err)
		}
		if err := common.CommitBlockStore(ctx, fx.bs, pid); err != nil {
			t.Fatalf("tail commit %q: %v", name, err)
		}
	}
	// Sample only: draining here would carry the tail to the remote and
	// dissolve the very pin phase 3 exists to observe.
	probeRounds(t, fx, ctx, "tail", 1, false)

	// PinnedBytes is a claim about what eviction cannot reclaim, so it has to be
	// checked against eviction while the pinned state still exists. Evicting
	// after a drain proves nothing: the drain carries the tail to the remote,
	// the pin dissolves, and eviction then frees everything — a green result
	// that never met the condition under test.
	t.Log("--- phase 3: does eviction actually leave PinnedBytes behind? ---")
	pre := fx.bs.GetStatsLite()
	if pre.PinnedBytes == 0 {
		t.Fatalf("nothing pinned before the evict pass, so this phase would be vacuous")
	}
	res, err := fx.bs.DrainLocalSynced(ctx)
	if err != nil {
		t.Fatalf("DrainLocalSynced: %v", err)
	}
	post := fx.bs.GetStatsLite()
	t.Logf("before evict: resident=%dMiB pinned_segs=%d pinned=%dMiB",
		pre.LocalDiskUsed>>20, pre.PinnedSegments, pre.PinnedBytes>>20)
	t.Logf("evict freed %d segments / %dMiB (held=%v)",
		res.SegmentsEvicted, res.BytesFreed>>20, res.Held)
	t.Logf("after evict:  resident=%dMiB pinned_segs=%d pinned=%dMiB",
		post.LocalDiskUsed>>20, post.PinnedSegments, post.PinnedBytes>>20)
	t.Logf("VERDICT: counter claimed %d bytes unreclaimable; eviction left %d behind (delta %d)",
		pre.PinnedBytes, post.LocalDiskUsed, post.LocalDiskUsed-pre.PinnedBytes)

	// Only now drain, so the run leaves nothing stranded in the bucket.
	if err := fx.bs.DrainAllUploads(ctx); err != nil {
		t.Fatalf("post-evict drain: %v", err)
	}
}

// probeRounds samples the residue against what it actually pins, drains, and
// repeats until the residue reaches zero or the rounds run out.
func probeRounds(t *testing.T, fx *byteVerifyFixture, ctx context.Context, phase string, rounds int, drain bool) {
	t.Helper()
	t.Logf("phase\tround\tunsynced_MiB\tresident_MiB\tsegs\tpinned_segs\tpinned_MiB\tworst_segs\tworst_MiB\tratio\tbarrier_ok")
	for round := 0; round < rounds; round++ {
		st := fx.bs.GetStatsLite()

		// dittofsWorstCasePinnedBytes, verbatim.
		var worstSegs int64
		if st.UnsyncedBytes > 0 {
			worstSegs = (st.UnsyncedBytes + probeStragglerBytes - 1) / probeStragglerBytes
		}
		worstBytes := worstSegs * probeSegmentBytes

		// The ratio: real pinned bytes over what the barrier budgets for. 1.0
		// means the worst case is the real case.
		ratio := "n/a"
		if worstBytes > 0 {
			ratio = fmt.Sprintf("%.4f", float64(st.PinnedBytes)/float64(worstBytes))
		}

		// dittofsDrainResidueOK, verbatim.
		barrierOK := st.UnsyncedBytes <= 0 ||
			st.LocalDiskUsed <= 64<<20 ||
			worstBytes <= st.LocalDiskUsed/5

		t.Logf("%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%s\t%v",
			phase, round, st.UnsyncedBytes>>20, st.LocalDiskUsed>>20, st.Segments,
			st.PinnedSegments, st.PinnedBytes>>20, worstSegs, worstBytes>>20,
			ratio, barrierOK)
		// Below a MiB the >>20 columns all read zero, so spell the small
		// residue out — it is the whole point of the tail phase.
		if st.UnsyncedBytes > 0 && st.UnsyncedBytes < 1<<20 {
			t.Logf("  residue=%dB pinned=%dB worst=%dB", st.UnsyncedBytes, st.PinnedBytes, worstBytes)
		}

		if st.UnsyncedBytes == 0 {
			t.Logf("residue reached zero at round %d", round)
			break
		}
		if !drain {
			return
		}

		drainStart := time.Now()
		if err := fx.bs.DrainAllUploads(ctx); err != nil {
			t.Fatalf("DrainAllUploads round %d: %v", round, err)
		}
		t.Logf("  drain round %d took %s", round, time.Since(drainStart).Round(time.Millisecond))
	}
}
