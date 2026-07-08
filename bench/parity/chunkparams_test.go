package parity

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/block/chunker"
)

// Opts.ChunkParams maps the --min-chunk/--max-chunk knobs onto a valid FastCDC
// profile (or the zero value the FSStore reads as default).
func TestOptsChunkParams(t *testing.T) {
	// Unset → zero Params (FSStore maps to DefaultParams).
	if got := (&Opts{}).ChunkParams(); got != (chunker.Params{}) {
		t.Fatalf("default: want zero Params, got %+v", got)
	}
	// Min only → Max defaults to the 16 MiB ceiling, Avg=Max, and it validates.
	p := (&Opts{MinChunk: 64 << 10}).ChunkParams()
	if p.Min != 64<<10 || p.Max != chunker.MaxChunkSize || p.Avg != p.Max {
		t.Fatalf("min-only: got %+v", p)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("min-only params rejected: %v", err)
	}
	// Min + Max both set.
	p2 := (&Opts{MinChunk: 128 << 10, MaxChunk: 256 << 10}).ChunkParams()
	if p2.Min != 128<<10 || p2.Max != 256<<10 || p2.Avg != 256<<10 {
		t.Fatalf("min+max: got %+v", p2)
	}
	if err := p2.Validate(); err != nil {
		t.Fatalf("min+max params rejected: %v", err)
	}
	// A too-small Min is rejected by Validate (surfaced through Opts.Validate).
	if err := (&Opts{MinChunk: 1024}).Validate(); err == nil {
		t.Fatal("expected sub-floor MinChunk to be rejected")
	}

	// Validate normalizes the persisted knobs to the effective params so the
	// scorecard RunOpts is self-describing.
	// --min-chunk alone → Max resolves to the 16 MiB ceiling.
	o := &Opts{MinChunk: 128 << 10}
	if err := o.Validate(); err != nil {
		t.Fatalf("validate min-only: %v", err)
	}
	if o.MinChunk != 128<<10 || o.MaxChunk != chunker.MaxChunkSize {
		t.Fatalf("min-only normalize: got Min=%d Max=%d", o.MinChunk, o.MaxChunk)
	}
	// --max-chunk without --min-chunk has no effect → cleared so it isn't persisted.
	o2 := &Opts{MaxChunk: 256 << 10}
	if err := o2.Validate(); err != nil {
		t.Fatalf("validate max-only: %v", err)
	}
	if o2.MinChunk != 0 || o2.MaxChunk != 0 {
		t.Fatalf("max-only normalize: got Min=%d Max=%d, want both 0", o2.MinChunk, o2.MaxChunk)
	}
}
