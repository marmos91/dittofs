//go:build linux || darwin

package segment_test

import (
	"path/filepath"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/platform"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// osStreamCap is the cap of the real-storage stream checks: small enough
// that a handful of records and a whole spare cost nothing, large enough
// that the filesystem reports whole blocks for it.
const osStreamCap = int64(1 << 20)

// TestStreamOnRealStorageAllocatesNoMoreThanAccounted checks R75 on the
// filesystem this test runs on: after a take and a run of appends through the
// platform's calls, the segment file's allocated bytes never exceed what the
// ledger accounts — the accounting never trails the filesystem in use — and
// every record lies inside what the account holds, each written once.
func TestStreamOnRealStorageAllocatesNoMoreThanAccounted(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	o := platform.NewOS(dir)
	set := segment.NewSet(osStreamCap, nil)
	// The maker's maximum is pinned at two caps — exactly the pool's two content
	// spares — so the refill the take kicks is refused by the maximum and
	// the footprint assertions below stand on the appends alone.
	maker := segment.NewMaker(o, set, 2*osStreamCap, 0, nil)
	pool := segment.NewPool(maker, segment.Targets{Content: 2}, nil)
	t.Cleanup(func() { pool.Stop() })
	pool.Start()
	awaitCounters(t, "a content spare ready", pool,
		func(c segment.PoolCounters) bool { return c.Spares >= 1 })
	budget := core.NewDescriptorBudget(64)
	cache := segment.NewDescriptors(o, format.SegmentName, budget)
	s := segment.NewStream(1, streamIdentity, set, pool, cache)
	t.Cleanup(func() {
		if err := cache.Shutdown(); err != nil {
			t.Errorf("the cache's Shutdown = %v, want nil", err)
		}
	})

	const records = 16
	var sum int64
	var locations []segment.Location
	for i := range records {
		payload := make([]byte, 512)
		for j := range payload {
			payload[j] = byte(i + j)
		}
		h := format.RecordHeader{
			Kind:    format.KindWrite,
			File:    streamFileID(i + 1),
			Extent:  core.Extent{Off: int64(i) * 512, Len: 512},
			Version: core.Version{Counter: uint64(i + 1)},
			Tag:     7,
		}
		body := format.RecordBody{Payload: payload, MTime: 1720000000000000000}
		p, err := s.Append(h, body)
		if err != nil {
			t.Fatalf("append %d: %v", i+1, err)
		}
		sum += p.Loc.Len
		locations = append(locations, p.Loc)
	}
	// The stream stays open until the read-back below: a closed stream
	// refuses reads.
	t.Cleanup(func() { _ = s.Close() })

	// The file's allocated bytes sit at or below the accounted size, which
	// the take raised to the cap before any storage was touched.
	path := filepath.Join(dir, format.SegmentName(1))
	ab := allocatedBytes(t, path)
	if c := set.Counters(); ab > c.UsedBytes {
		t.Fatalf("the segment file holds %d allocated bytes, more than the %d the ledger accounts", ab, c.UsedBytes)
	}
	// The take accounted the segment at its cap, and no append moved the
	// segments' own footprint: the pool's ready spares sit beside it.
	if c := set.Counters(); c.UsedBytes-c.Spares*osStreamCap != osStreamCap {
		t.Fatalf("the segments account for %d, want the take's cap %d: the appends must not move it", c.UsedBytes-c.Spares*osStreamCap, osStreamCap)
	}
	if got := set.ShareBytes(7); got != sum {
		t.Fatalf("the share holds %d record bytes, want the %d the appends wrote", got, sum)
	}

	// Every record reads back whole and byte for byte on the real file.
	for i, loc := range locations {
		raw := make([]byte, loc.Len)
		if _, err := s.ReadAt(1, raw, loc.Off); err != nil {
			t.Fatalf("ReadAt of record %d: %v", i+1, err)
		}
		h, err := format.DecodeRecordHeader(raw[:format.RecordHeaderSize], streamIdentity, 1, loc.Off)
		if err != nil {
			t.Fatalf("record %d does not decode: %v", i+1, err)
		}
		body := make([]byte, h.BodySize())
		if _, err := s.ReadAt(1, body, loc.Off+format.RecordHeaderSize); err != nil {
			t.Fatalf("ReadAt of record %d's body: %v", i+1, err)
		}
		b, err := format.VerifyRecordBody(body, streamIdentity, h)
		if err != nil {
			t.Fatalf("record %d's body does not verify: %v", i+1, err)
		}
		for j, x := range b.Payload {
			if want := byte(i + j); x != want {
				t.Fatalf("record %d's body byte %d is %d, want %d: the read served other bytes than the append wrote", i+1, j, x, want)
			}
		}
	}
}
