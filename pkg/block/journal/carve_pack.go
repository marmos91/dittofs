package journal

import (
	"context"
	"math"

	"golang.org/x/sync/errgroup"
)

// runState carries one dirty run through a carve pass: its live intervals (as
// widened by extendRunToRowEnd), how far its records have been flipped synced,
// and the file offsets the fresh tiling produced. flipIdx is advanced only by
// the dispatcher's flipping worker, one at a time via the prev/mine chain;
// newOffsets is written only by the single packer goroutine.
type runState struct {
	ivs        []interval
	flipIdx    int
	newOffsets map[int64]struct{}
}

func (r *runState) start() int64 { return r.ivs[0].fileOff }
func (r *runState) end() int64   { return r.ivs[len(r.ivs)-1].end() }

// complete reports whether every one of this run's records flipped synced, which
// is the precondition for reaping the rows the run superseded.
func (r *runState) complete() bool { return r.flipIdx == len(r.ivs) }

// flipPlan names the runs one packed block contributed to. A block flushes at
// CarveBlockSize, so it may cover the tail of one run, several whole runs, and a
// prefix of the last: runs first..last-1 flip to their own end, run last flips
// only up to lastOff.
type flipPlan struct {
	first, last int
	lastOff     int64
}

// resolveRunExtents widens each run to end on a manifest row boundary, so the
// reap that follows never deletes a row straddling the run's tail. The
// ManifestRowEndAfter lookups are a per-run metadata round trip, so they fan out
// under the same bound as block commits.
func (s *Store) resolveRunExtents(ctx context.Context, sh *shard, id FileID, rs []*runState) error {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(s.cfg.CarveUploadConcurrency)
	for i := range rs {
		limit := int64(math.MaxInt64)
		if i+1 < len(rs) {
			limit = rs[i+1].start()
		}
		g.Go(func() error {
			ivs, err := s.extendRunToRowEnd(gctx, sh, id, rs[i].ivs, limit)
			if err != nil {
				return err
			}
			rs[i].ivs = ivs
			return nil
		})
	}
	return g.Wait()
}
