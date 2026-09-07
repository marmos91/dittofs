package journal

import (
	"context"
	"fmt"
	"testing"
)

// Running these: six of them rebuild their state inside StopTimer on every
// iteration — Carve, CarveScatteredPass, Evict, GCRepack, Truncate and Delete.
// Go sizes b.N from the timed region alone, so on a fast device it picks an
// iteration count whose *untimed* setup runs for tens of minutes; Truncate
// reached 28k iterations on tmpfs, each rebuilding 4096 intervals. Give that
// group an explicit iteration cap rather than a duration:
//
//	go test -run '^$' -bench . -benchtime=1s -count=6                        # the rest
//	go test -run '^$' -bench 'Carve|Evict|GCRepack|Truncate|Delete' \
//	        -benchtime=200x -count=6                                          # this group
//
// ns/op stays directly comparable between the two invocations; only the sample
// count differs.
//
// This file completes the benchmark contract: one benchmark per state
// transition, so the benchmark set and the state model are the same list. The
// write path was already covered; everything a byte does after it is written —
// going cold, coming back, being reclaimed, surviving a restart — was not, and
// an extraction can lose that behaviour without a single number moving.

// benchStoreDir is benchStore plus the store directory, which the on-disk
// write-amplification metric needs and the recovery benchmark reopens.
func benchStoreDir(b *testing.B, cfg Config) (*Store, string) {
	b.Helper()
	dir := b.TempDir()
	s, err := Open(dir, cfg, newFakeRemote(), SystemClock())
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	b.Cleanup(func() { _ = s.Close() })
	return s, dir
}

// intervalCount reports how many index intervals a file holds, taking the shard
// lock rather than reading the map bare: a parallel benchmark otherwise races
// its own store.
func intervalCount(s *Store, id FileID) int {
	sh := s.shardFor(id)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	fi := sh.index[id]
	if fi == nil {
		return 0
	}
	return len(fi.ivs)
}

// coldIntervalCount reports how many of a file's intervals are cold. Counting
// intervals alone cannot tell a seeded-cold file from a written one — both hold
// the same number — so any check that a range is genuinely remote-only has to
// look at the flag.
func coldIntervalCount(s *Store, id FileID) int {
	sh := s.shardFor(id)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	fi := sh.index[id]
	if fi == nil {
		return 0
	}
	n := 0
	for _, iv := range fi.ivs {
		if iv.cold {
			n++
		}
	}
	return n
}

// reportWriteAmp attaches the three write-path metrics to any benchmark that
// appends: segment count, write amplification (on-disk bytes over payload
// bytes) and the file's interval count. Write amplification is the number that
// decides whether the storage design is viable at all, so it belongs on every
// benchmark that writes, not only the random-write pair it started on.
func reportWriteAmp(b *testing.B, s *Store, dir string, id FileID, payloadBytes int64) {
	b.Helper()
	st := s.Stats()
	b.ReportMetric(float64(st.Segments), "segments")
	if payloadBytes > 0 {
		b.ReportMetric(float64(diskBytesOnDisk(b, dir))/float64(payloadBytes), "write-amp")
	}
	b.ReportMetric(float64(intervalCount(s, id)), "intervals")
}

// BenchmarkReadCold measures the cold-read resolution path: the index knows the
// bytes were written and evicted, so ReadAt zero-fills and reports Cold for the
// caller to hydrate. It is the read half of every silent-zeros incident this
// store has had, and until now it had no benchmark at all — a regression here
// shows up as corruption, not as a slow test.
func BenchmarkReadCold(b *testing.B) {
	s := benchStore(b)
	ctx := context.Background()
	const (
		chunk = 64 << 10
		spans = 256
	)
	ext := make([][2]int64, spans)
	for i := range ext {
		ext[i] = [2]int64{int64(i) * chunk, chunk}
	}
	if err := s.SeedCold(ctx, "cold", ext); err != nil {
		b.Fatalf("SeedCold: %v", err)
	}

	dst := make([]byte, chunk)
	b.SetBytes(chunk)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, st, err := s.ReadAt(ctx, "cold", int64(i%spans)*chunk, dst)
		if err != nil {
			b.Fatalf("ReadAt: %v", err)
		}
		if !st.Cold {
			b.Fatal("read resolved warm: the benchmark is measuring the wrong path")
		}
	}
}

// BenchmarkHydrate measures Remote -> Resident: the local half of a cold fill,
// appending fetched bytes back under the version fence. Each group of spans
// gets a fresh file because SeedCold only fills holes — re-seeding a hydrated
// range is a no-op, which would silently turn this into a warm-write benchmark.
func BenchmarkHydrate(b *testing.B) {
	s := benchStore(b)
	ctx := context.Background()
	const (
		chunk = 64 << 10
		spans = 64
	)
	ext := make([][2]int64, spans)
	for i := range ext {
		ext[i] = [2]int64{int64(i) * chunk, chunk}
	}
	buf := make([]byte, chunk)

	b.SetBytes(chunk)
	b.ReportAllocs()
	b.ResetTimer()
	var id FileID
	for i := 0; i < b.N; i++ {
		if i%spans == 0 {
			b.StopTimer()
			id = FileID(fmt.Sprintf("fill-%d", i/spans))
			if err := s.SeedCold(ctx, id, ext); err != nil {
				b.Fatalf("SeedCold: %v", err)
			}
			// SeedCold only fills holes. If it ever stopped taking, every Hydrate
			// below would be an ordinary append and this would quietly become a
			// second write benchmark.
			if got := coldIntervalCount(s, id); got != spans {
				b.Fatalf("seeded %d cold intervals, want %d", got, spans)
			}
			b.StartTimer()
		}
		if err := s.Hydrate(ctx, id, int64(i%spans)*chunk, buf, 0); err != nil {
			b.Fatalf("Hydrate: %v", err)
		}
	}
}

// BenchmarkEvict measures Resident -> Remote: reclaiming a whole sealed segment
// under storage pressure. Only fully-synced segments qualify, so the fill uses
// Hydrate, which appends records already marked synced; a WriteAt fill would
// measure a pass that correctly declines to evict anything.
//
// The refill runs once per iteration and writes just past a segment's worth, so
// the rotation it forces leaves exactly one sealed segment for the Evict that
// follows. Filling several iterations ahead does not work — a refill's bytes
// coalesce into a single active segment, so the second Evict onwards finds
// nothing and times an empty pass.
func BenchmarkEvict(b *testing.B) {
	s, _ := benchStoreDir(b, Config{SegmentSize: minSegmentSize, ShardCount: 1})
	ctx := context.Background()
	const span = 128 << 10
	perFill := int(minSegmentSize/span) + 1
	buf := make([]byte, span)
	fill := func(round int) {
		for j := 0; j < perFill; j++ {
			id := FileID(fmt.Sprintf("evict-%d-%d", round, j))
			if err := s.Hydrate(ctx, id, 0, buf, 0); err != nil {
				b.Fatalf("Hydrate: %v", err)
			}
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		fill(i)
		b.StartTimer()
		// targetBytes <= 0 evicts a single qualifying segment, which is the unit
		// this benchmark is timing.
		res, err := s.Evict(ctx, 0)
		if err != nil {
			b.Fatalf("Evict: %v", err)
		}
		// A pass that qualifies nothing returns cleanly and costs almost nothing,
		// so without this the benchmark would keep reporting a fast, meaningless
		// number if the fill ever stopped producing evictable segments.
		if res.SegmentsEvicted == 0 {
			b.Fatal("nothing evicted: the benchmark is timing an empty pass")
		}
	}
}

// BenchmarkGCRepack measures the compaction transition: relocating a sealed
// segment's still-live records into a fresh one and retiring the victim. The
// per-iteration seed is the same shape gc_test uses — 300 KiB kept, 700 KiB
// deleted, a rollover to seal it — which clears the forced dead-byte ratio.
func BenchmarkGCRepack(b *testing.B) {
	s, _ := benchStoreDir(b, Config{SegmentSize: minSegmentSize, ShardCount: 1})
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		seedRepackable(b, s, true)
		b.StartTimer()
		res, err := s.GC(ctx, GCOptions{Force: true})
		if err != nil {
			b.Fatalf("GC: %v", err)
		}
		if res.SegmentsRepacked == 0 {
			b.Fatal("nothing repacked: the benchmark is timing an empty pass")
		}
		b.StopTimer()
		if err := s.Delete(ctx, "keep"); err != nil {
			b.Fatalf("Delete: %v", err)
		}
		b.StartTimer()
	}
}

// BenchmarkOpenRecovery measures replaying a populated store directory at
// startup. This is a startup-latency SLO, not a curiosity: recovery walks every
// segment's records to rebuild the interval index, so its cost scales with what
// the share holds, and a share that takes minutes to open is indistinguishable
// from one that is down.
func BenchmarkOpenRecovery(b *testing.B) {
	const (
		files = 512
		span  = 64 << 10
	)
	dir := b.TempDir()
	seed, err := Open(dir, Config{}, newFakeRemote(), SystemClock())
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	data := make([]byte, span)
	for i := 0; i < files; i++ {
		if err := seed.WriteAt(ctx, FileID(fmt.Sprintf("rec-%d", i)), 0, data); err != nil {
			b.Fatalf("seed WriteAt: %v", err)
		}
	}
	if err := seed.Close(); err != nil {
		b.Fatalf("Close: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s, err := Open(dir, Config{}, newFakeRemote(), SystemClock())
		if err != nil {
			b.Fatalf("Open: %v", err)
		}
		b.StopTimer()
		if got := s.FileCount(); got != files {
			b.Fatalf("recovered %d files, want %d", got, files)
		}
		if err := s.Close(); err != nil {
			b.Fatalf("Close: %v", err)
		}
		b.StartTimer()
	}
}

// BenchmarkTruncate measures clipping a file's tail: a marker append plus the
// index clip over a deep interval list, which is where the cost lives.
func BenchmarkTruncate(b *testing.B) {
	s := benchStore(b)
	ctx := context.Background()
	const (
		chunk = 4 << 10
		spans = 4096
	)
	data := make([]byte, chunk)
	size := int64(spans) * chunk
	refill := func() {
		for j := 0; j < spans; j++ {
			if err := s.WriteAt(ctx, "trunc", int64(j)*chunk, data); err != nil {
				b.Fatalf("WriteAt: %v", err)
			}
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		refill()
		b.StartTimer()
		if err := s.Truncate(ctx, "trunc", size/2); err != nil {
			b.Fatalf("Truncate: %v", err)
		}
	}
}

// BenchmarkDelete measures tombstoning a file with a deep interval list: the
// index drop plus the dead-byte charge against every segment it touched.
func BenchmarkDelete(b *testing.B) {
	s := benchStore(b)
	ctx := context.Background()
	const (
		chunk = 4 << 10
		spans = 1024
	)
	data := make([]byte, chunk)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		id := FileID(fmt.Sprintf("del-%d", i))
		for j := 0; j < spans; j++ {
			if err := s.WriteAt(ctx, id, int64(j)*chunk, data); err != nil {
				b.Fatalf("WriteAt: %v", err)
			}
		}
		b.StartTimer()
		if err := s.Delete(ctx, id); err != nil {
			b.Fatalf("Delete: %v", err)
		}
	}
}

// The four residency queries below are the receipts for the merged Extents
// query the pier design proposes. Collapsing them into one slice-returning call
// is that design's biggest performance risk — ColdExtents is O(live intervals)
// across every shard — so the before-numbers have to exist on the same hardware
// before the after-numbers mean anything. Recorded now; compared when Extents
// lands.
//
// FileSize is the one to watch, but not because it is on the read path: it is
// reached only from share start, and it is an O(intervals) scan that costs
// three orders of magnitude more than DurableExtent for a narrower answer.
// Measuring the four separately is what makes that visible.

// residencyStore builds a file deep enough in intervals for the query cost to
// be visible, with a cold tail so ColdExtents has something to walk.
func residencyStore(b *testing.B) (*Store, FileID, int64) {
	b.Helper()
	s := benchStore(b)
	ctx := context.Background()
	const (
		chunk = 4 << 10
		spans = 4096
	)
	data := make([]byte, chunk)
	// A gap between spans keeps the intervals from merging into one run, which
	// is what makes the index deep rather than trivially short.
	for j := 0; j < spans; j++ {
		if err := s.WriteAt(ctx, "res", int64(j)*chunk*2, data); err != nil {
			b.Fatalf("WriteAt: %v", err)
		}
	}
	size := int64(spans) * chunk * 2
	cold := make([][2]int64, spans)
	for j := range cold {
		cold[j] = [2]int64{int64(j)*chunk*2 + chunk, chunk}
	}
	if err := s.SeedCold(ctx, "res", cold); err != nil {
		b.Fatalf("SeedCold: %v", err)
	}
	return s, "res", size
}

func BenchmarkFileSize(b *testing.B) {
	s, id, _ := residencyStore(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := s.FileSize(ctx, id); !ok {
			b.Fatal("FileSize: file not found")
		}
	}
}

func BenchmarkDataExtents(b *testing.B) {
	s, id, size := residencyStore(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.DataExtents(ctx, id, size); err != nil {
			b.Fatalf("DataExtents: %v", err)
		}
	}
}

func BenchmarkDurableExtent(b *testing.B) {
	s, id, _ := residencyStore(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.DurableExtent(ctx, id)
	}
}

func BenchmarkColdExtents(b *testing.B) {
	s, _, _ := residencyStore(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := s.ColdExtents(ctx); err != nil {
			b.Fatalf("ColdExtents: %v", err)
		}
	}
}
