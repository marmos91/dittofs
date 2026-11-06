package durability

import (
	"context"
	"errors"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
)

// FuzzSyncInterleaving drives append, Sync, a Sync with a cancelled context
// and a scheduled sync failure over two files on one stream, and checks what
// each Sync may return against what the test knows happened:
//   - nil only if every earlier record of the file is Durable;
//   - a SyncError only if a failure was scheduled while one of the file's
//     records was unsynced, or on a segment a failure was scheduled for;
//   - the context's error only for the cancelled call.
func FuzzSyncInterleaving(f *testing.F) {
	f.Add([]byte{0, 1})
	f.Add([]byte{0, 4, 1, 5})
	f.Add([]byte{0, 3, 1, 4, 1})
	f.Add([]byte{0, 2, 1, 6, 0, 1})
	f.Fuzz(func(t *testing.T, ops []byte) {
		if len(ops) > 24 {
			ops = ops[:24]
		}
		r := newRig(t, 1)
		files := [2]core.FileID{file(1), file(2)}
		var locs [2][]segment.Location
		var off int64
		maybeFailed := map[core.SegmentID]bool{}
		var last segment.Location
		appended := false

		for _, op := range ops {
			i := int(op>>2) & 1
			switch op & 3 {
			case 0:
				last = r.appendWrite(t, files[i], off, "x")
				off += 8
				appended = true
				locs[i] = append(locs[i], last)
			case 1:
				err := r.d.Sync(context.Background(), files[i])
				switch {
				case err == nil:
					for _, l := range locs[i] {
						if !r.d.Durable(l) {
							t.Fatalf("Sync of file %d returned nil with %+v not durable", i, l)
						}
					}
				case errors.Is(err, core.ErrSyncFailed):
					hit := false
					for _, l := range locs[i] {
						hit = hit || (!r.d.Durable(l) && maybeFailed[l.Segment])
					}
					if !hit {
						t.Fatalf("Sync of file %d failed with no failure scheduled for it: %v", i, err)
					}
				default:
					t.Fatalf("Sync of file %d returned %v", i, err)
				}
			case 2:
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				if err := r.d.Sync(ctx, files[i]); !errors.Is(err, context.Canceled) {
					t.Fatalf("a cancelled Sync returned %v, want context.Canceled", err)
				}
			case 3:
				if !appended {
					continue
				}
				rule := journaltest.Failing(journaltest.OpSync, format.SegmentName(last.Segment), 1, errors.New("injected"))
				if err := r.seam.Schedule(rule); err != nil {
					t.Fatal(err)
				}
				maybeFailed[last.Segment] = true
			}
		}
	})
}
