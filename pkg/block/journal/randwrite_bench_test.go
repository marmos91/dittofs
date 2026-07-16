package journal

import (
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// diskBytesOnDisk sums the on-disk size of every .seg file under the store dir.
// It is the write-amplification numerator: total bytes the store persisted
// (headers + fileIDs + payloads + CRCs), against the payload bytes the caller
// asked to write.
func diskBytesOnDisk(b *testing.B, dir string) int64 {
	b.Helper()
	var total int64
	entries, err := os.ReadDir(dir)
	if err != nil {
		b.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != segSuffix {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			b.Fatalf("Info: %v", err)
		}
		total += fi.Size()
	}
	return total
}

// benchWrites drives b.N 4 KiB writes into a single file, picking each offset
// from offsets[i%len] — a random-shuffled slice for the random case, a strictly
// increasing slice for the sequential case. Same syscall count and same payload
// per op in both; the ONLY difference is where each write lands in the file, so
// any gap between the two isolates the offset-dependent cost (index insertion
// position). Reports ns/op plus segments and write-amp as custom metrics.
func benchWrites(b *testing.B, offsets []int64) {
	dir := b.TempDir()
	s, err := Open(dir, Config{}, newFakeRemote(), SystemClock())
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	b.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	data := make([]byte, 4<<10)

	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.WriteAt(ctx, "f", offsets[i%len(offsets)], data); err != nil {
			b.Fatalf("WriteAt: %v", err)
		}
	}
	b.StopTimer()

	disk := diskBytesOnDisk(b, dir)
	st := s.Stats()
	b.ReportMetric(float64(st.Segments), "segments")
	b.ReportMetric(float64(disk)/float64(int64(b.N)*int64(len(data))), "write-amp")
	b.ReportMetric(float64(len(s.shardFor("f").index["f"].ivs)), "intervals")
}

const randSpan = 1 << 20 // 1 Mi distinct 4 KiB slots => a 4 GiB address space

// BenchmarkRandWrite4K writes 4 KiB records at pseudo-random distinct offsets:
// each op inserts into the MIDDLE of the file's sorted interval index.
func BenchmarkRandWrite4K(b *testing.B) {
	offs := make([]int64, randSpan)
	for i := range offs {
		offs[i] = int64(i) * (4 << 10)
	}
	rng := rand.New(rand.NewSource(1))
	rng.Shuffle(len(offs), func(i, j int) { offs[i], offs[j] = offs[j], offs[i] })
	benchWrites(b, offs)
}

// BenchmarkRandWriteBounded models a real fio randwrite: 4 KiB writes at random
// offsets within a FIXED-size file, so the index fills to fileBlocks intervals
// and then every further write is an in-place overwrite (the steady state). This
// is the rig-representative shape — bounded index, mixed fill+overwrite — where
// the per-insert allocation/sort overhead (not the extreme-N memmove) dominates.
func BenchmarkRandWriteBounded(b *testing.B) {
	const fileBlocks = 4096 // 16 MiB working set => a rig-like interval count
	offs := make([]int64, fileBlocks)
	for i := range offs {
		offs[i] = int64(i) * (4 << 10)
	}
	rng := rand.New(rand.NewSource(1))
	rng.Shuffle(len(offs), func(i, j int) { offs[i], offs[j] = offs[j], offs[i] })
	seq := make([]int64, b.N)
	for i := range seq {
		seq[i] = offs[rng.Intn(len(offs))]
	}
	benchWrites(b, seq)
}

// BenchmarkSeqWrite4K writes 4 KiB records at strictly increasing offsets:
// each op appends to the END of the sorted interval index. Same everything else
// as BenchmarkRandWrite4K — the control.
func BenchmarkSeqWrite4K(b *testing.B) {
	offs := make([]int64, randSpan)
	for i := range offs {
		offs[i] = int64(i) * (4 << 10)
	}
	benchWrites(b, offs)
}
