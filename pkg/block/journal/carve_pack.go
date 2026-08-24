package journal

import (
	"context"
	"math"

	"golang.org/x/sync/errgroup"
)

// runState carries one dirty run through a carve pass: its live intervals, as
// widened by extendRunToRowEnd.
type runState struct {
	ivs []interval
}

func (r *runState) start() int64 { return r.ivs[0].fileOff }

// resolveRunExtents widens each run to end on a manifest row boundary, so the
// reap that follows never deletes a row straddling the run's tail. The
// ManifestRowEndAfter lookups are a per-run metadata round trip, so they fan out
// under the same bound as carveFile's own block commits below.
//
// ponytail: reuses the CarveUploadConcurrency knob carveFile also uses for its
// own dispatch; give the two phases separate limits only if profiling shows
// upload and metadata fan-out need to diverge.
func (s *Store) resolveRunExtents(ctx context.Context, sh *shard, id FileID, rs []*runState) error {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(s.cfg.CarveUploadConcurrency)
	for i := range rs {
		// limit is read here, on the launching goroutine, before g.Go — every
		// run is materialised into rs before any is dispatched, so rs[i+1] is
		// never written concurrently, but reading it *inside* the closure
		// instead would race the goroutine that later mutates rs[i+1].ivs.
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
