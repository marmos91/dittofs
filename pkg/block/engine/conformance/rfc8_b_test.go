//go:build engine_conformance

package conformance

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// secondFile creates a file of the fixture's second share.
func secondFile(t *testing.T, f *enginetest.Fixture, name string) entity.FileID {
	t.Helper()

	_, file := f.FS.Create(t, f.FS.Admin, f.SecondRoot, name, kvstore.Regular(0o644))

	return file.ID
}

// TestRFC8B_SharedJournal: two shares on one device journal, one filled to
// its journal limit, and the other's writes are not refused.
func TestRFC8B_SharedJournal(t *testing.T) {
	f := enginetest.New(t)
	ctx := t.Context()
	second := f.Second
	second.Limit = 64 << 10

	if err := f.E.Bind(ctx, second); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	full := secondFile(t, f, "full")
	p := make([]byte, 16<<10)
	filled := false

	for i := range 16 {
		_, err := f.E.Write(ctx, second.ID, full, int64(i*len(p)), p, engine.WriteOpts{Done: func() {}})
		if err == nil {
			continue
		}

		var d *vfserr.Delay

		if !errors.As(err, &d) || d.Cause != vfserr.DelayCapacity {
			t.Fatalf("the filling write: %v, want ErrDelay(DelayCapacity) at the limit", err)
		}

		filled = true

		break
	}

	if !filled {
		t.Fatal("the second share never reached its journal limit")
	}

	_, id := f.File("other")
	write(t, f, id, 0, []byte("the other share is served"))
}

// p99 is the 99th percentile of ds.
func p99(ds []time.Duration) time.Duration {
	s := slices.Clone(ds)
	slices.Sort(s)

	return s[len(s)*99/100]
}

// writeLatencies times n writes of 4 KiB to the fixture's share.
func writeLatencies(t *testing.T, f *enginetest.Fixture, name string, n int) []time.Duration {
	t.Helper()

	_, id := f.File(name)
	p := make([]byte, 4<<10)
	ds := make([]time.Duration, 0, n)

	for i := range n {
		start := time.Now()
		write(t, f, id, int64(i*len(p)), p)
		ds = append(ds, time.Since(start))
	}

	return ds
}

// TestRFC8B_NoCrossShareSlowdown, the write half: one share saturated with
// writes leaves the other's p99 write latency within its unloaded baseline's
// bound, twice the baseline or the baseline plus a millisecond, whichever is
// larger. The engine exposes no lock-wait series through Stats yet, so the
// row checks latency alone.
func TestRFC8B_NoCrossShareSlowdown(t *testing.T) {
	f := enginetest.New(t)
	ctx := t.Context()

	if err := f.E.Bind(ctx, f.Second); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	base := p99(writeLatencies(t, f, "baseline", 200))
	loads := make([]entity.FileID, 4)

	for w := range loads {
		loads[w] = secondFile(t, f, "load"+string(rune('a'+w)))
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(len(loads))

	for _, id := range loads {
		go func() {
			defer wg.Done()

			p := make([]byte, 4<<10)

			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}

				_, err := f.E.Write(ctx, f.Second.ID, id, int64(i%256*len(p)), p, engine.WriteOpts{Done: func() {}})
				if err != nil {
					t.Errorf("a loading write: %v", err)

					return
				}
			}
		}()
	}

	loaded := p99(writeLatencies(t, f, "loaded", 200))
	close(stop)
	wg.Wait()

	if bound := max(2*base, base+time.Millisecond); loaded > bound {
		t.Errorf("p99 write latency %v under the other share's load, want within %v (baseline %v)", loaded, bound, base)
	}
}
