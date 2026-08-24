package journal

import (
	"context"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/chunker"
)

// pipelineSink wraps the plain fake sink with the optional manifest-row-end
// capability and records the order in which a carve pass asks for row ends
// versus commits blocks. The real sink answers ManifestRowEndAfter out of the
// metadata store with a whole-manifest listing, so a pass that resolves every
// run's extent before committing anything pays that cost N times over with
// nothing durable to show for it. The ordering is the observable that stands in
// for the cost.
type pipelineSink struct {
	*fakeSink
	mu     sync.Mutex
	events []string
}

func (p *pipelineSink) ManifestRowEndAfter(_ context.Context, _ FileID, off int64) (int64, error) {
	p.mu.Lock()
	p.events = append(p.events, "rowend")
	p.mu.Unlock()
	return off, nil // no extension: this test is about ordering, not widening
}

func (p *pipelineSink) CommitBlock(ctx context.Context, chunks []CarveChunk) error {
	err := p.fakeSink.CommitBlock(ctx, chunks)
	p.mu.Lock()
	p.events = append(p.events, "commit")
	p.mu.Unlock()
	return err
}

func (p *pipelineSink) snapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.events...)
}

// TestCarveInterleavesRowEndLookupsWithCommits pins that a carve pass makes a
// block durable before it has finished asking the metadata store about every
// dirty run. Each row-end lookup is a whole-manifest read, so a pass that
// resolves all of them up front produces no durable byte — and no drop in the
// unsynced counter — for the whole of that prologue. On a scattered dirty set
// the prologue is thousands of lookups long, which reads from outside as a
// wedged drain rather than a slow one.
func TestCarveInterleavesRowEndLookupsWithCommits(t *testing.T) {
	const (
		cells     = 400
		cell      = 4 << 10
		runs      = 100 // every fourth cell is overwritten
		blockSize = 64 << 10
		// One block's worth of runs, doubled for slack: the first commit must
		// land within roughly the prologue one block genuinely needs, not merely
		// somewhere before the last lookup.
		maxPrologue = 2 * (blockSize / cell)
	)
	s, dd, fs, _ := carveStore(t, Config{
		CarveBlockSize:         blockSize,
		CarveUploadConcurrency: 4,
		ChunkParams:            chunker.Params{Min: 1 << 10, Avg: 2 << 10, Max: 8 << 10},
	})
	ctx := context.Background()

	// Lay the base down one cell at a time so each becomes its own interval, then
	// carve it warm. Overwriting a whole cell later leaves its neighbours warm and
	// standing, which is what makes the row-end lookup fire for every dirty run —
	// a partial overwrite would re-dirty the survivors and merge the runs instead.
	for i := 0; i < cells; i++ {
		if err := s.WriteAt(ctx, "f", int64(i)*cell, randBytes(cell, int64(i))); err != nil {
			t.Fatalf("WriteAt base %d: %v", i, err)
		}
	}
	if _, err := s.Carve(ctx, CarveOptions{Force: true}); err != nil {
		t.Fatalf("Carve base: %v", err)
	}

	ps := &pipelineSink{fakeSink: fs}
	s.SetCarveTargets(dd, ps)

	for i := 0; i < runs; i++ {
		off := int64(i) * 4 * cell
		if err := s.WriteAt(ctx, "f", off, randBytes(cell, int64(1000+i))); err != nil {
			t.Fatalf("WriteAt %d: %v", i, err)
		}
	}
	if _, err := s.Carve(ctx, CarveOptions{Force: true}); err != nil {
		t.Fatalf("Carve scattered: %v", err)
	}

	ev := ps.snapshot()
	prologue, total, committed := 0, 0, false
	for _, e := range ev {
		switch e {
		case "commit":
			committed = true
		case "rowend":
			total++
			if !committed {
				prologue++
			}
		}
	}
	if total == 0 {
		t.Fatalf("no row-end lookup happened: fixture does not exercise the path")
	}
	if !committed {
		t.Fatalf("no block committed")
	}
	if prologue > maxPrologue {
		t.Fatalf("carve front-loaded run-extent resolution: %d row-end lookups "+
			"before the first commit (of %d runs), want at most %d",
			prologue, runs, maxPrologue)
	}
}
