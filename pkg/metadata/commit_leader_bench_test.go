package metadata

import (
	"context"
	"os"
	"strconv"
	"sync"
	"testing"
)

// BenchmarkCommitLeader_Concurrent measures the durable-commit throughput of the
// group-commit coordinator under concurrency, against the wall it removes: a
// real fsync barrier. Each iteration is one durable COMMIT (the metadata
// flushPendingWrite tail). "direct" is the pre-#1573 behaviour — every
// concurrent committer issues its own fsync; "coalesced" routes them through the
// commitLeader so a concurrent burst collapses onto one fsync.
//
// The barrier fsyncs a real file so the wall shows: on a filesystem that honors
// fsync (the Linux bench VM) "direct" is fsync-bound at ~one journal commit per
// op while "coalesced" amortizes N ops onto ~one, so the win grows with
// concurrency. NOTE: on macOS fsync is NOT a full barrier (Darwin's fsync
// returns before the platter write; F_FULLFSYNC would be needed), so the local
// ratio understates the win — mirror this on Linux to see the real number.
func BenchmarkCommitLeader_Concurrent(b *testing.B) {
	for _, conc := range []int{1, 4, 16, 64} {
		f, err := os.CreateTemp(b.TempDir(), "barrier")
		if err != nil {
			b.Fatal(err)
		}
		barrier := func() error { return f.Sync() }
		b.Cleanup(func() { _ = f.Close() })

		b.Run("direct/conc="+strconv.Itoa(conc), func(b *testing.B) {
			runConcurrent(b, conc, func(context.Context) error { return barrier() })
		})

		l := newCommitLeader(barrier)
		b.Run("coalesced/conc="+strconv.Itoa(conc), func(b *testing.B) {
			runConcurrent(b, conc, l.Sync)
		})
	}
}

// runConcurrent drives b.N total durable commits spread over conc goroutines and
// reports ns/op (so ops/s = 1e9/ns-op) at that concurrency.
func runConcurrent(b *testing.B, conc int, commit func(context.Context) error) {
	b.ResetTimer()
	var wg sync.WaitGroup
	per := b.N / conc
	extra := b.N % conc
	for g := 0; g < conc; g++ {
		n := per
		if g < extra {
			n++
		}
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for i := 0; i < n; i++ {
				if err := commit(context.Background()); err != nil {
					b.Error(err)
					return
				}
			}
		}(n)
	}
	wg.Wait()
}
