package badgerkv

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/kv"
)

// BenchmarkDurableCommits measures single-key synced commits from 1, 16 and 64
// writers against the device's own sync rate in the same directory. Concurrent
// commits must share syncs: at 64 writers commits/s should be many times the
// device's syncs/s, where a store syncing per commit stays near 1x. The test
// temp dir is often tmpfs, where a sync costs nothing; set KV_BENCH_DIR to a
// directory on the device under test.
func BenchmarkDurableCommits(b *testing.B) {
	dir := benchDir(b)
	syncs := deviceSyncsPerSec(b, dir)
	for _, writers := range []int{1, 16, 64} {
		b.Run(fmt.Sprintf("writers=%d", writers), func(b *testing.B) {
			s, err := Open(Options{Dir: benchDir(b)})
			if err != nil {
				b.Fatal(err)
			}
			defer func() {
				if err := s.Close(); err != nil {
					b.Error(err)
				}
			}()
			var wg sync.WaitGroup
			start := time.Now()
			b.ResetTimer()
			for w := range writers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := w; i < b.N; i += writers {
						if err := s.Update(context.Background(), func(tx kv.Txn) error {
							return tx.Set(fmt.Appendf(nil, "w%d", w), []byte("v"))
						}); err != nil {
							b.Error(err)
							return
						}
					}
				}()
			}
			wg.Wait()
			rate := float64(b.N) / time.Since(start).Seconds()
			b.ReportMetric(rate, "commits/s")
			b.ReportMetric(syncs, "device-syncs/s")
			b.ReportMetric(rate/syncs, "x-device")
		})
	}
}

func benchDir(b *testing.B) string {
	root := os.Getenv("KV_BENCH_DIR")
	if root == "" {
		return b.TempDir()
	}
	dir, err := os.MkdirTemp(root, "kvbench")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			b.Error(err)
		}
	})
	return dir
}

func deviceSyncsPerSec(b *testing.B, dir string) float64 {
	f, err := os.Create(filepath.Join(dir, "sync"))
	if err != nil {
		b.Fatal(err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			b.Error(err)
		}
	}()
	buf := make([]byte, 4096)
	n, start := 0, time.Now()
	for time.Since(start) < time.Second {
		if _, err := f.WriteAt(buf, 0); err != nil {
			b.Fatal(err)
		}
		if err := f.Sync(); err != nil {
			b.Fatal(err)
		}
		n++
	}
	return float64(n) / time.Since(start).Seconds()
}
