package model

import (
	"math"
	"math/rand/v2"
)

// weights is how often Generate draws each operation, out of their sum.
// Writes dominate, as they do a journal's load; reads are frequent because a
// fill is made as of the change version the last read reported.
var weights = [...]int{
	OpWrite: 30, OpSync: 8, OpRead: 10, OpFill: 6, OpStaleFill: 3, OpOffload: 8, OpRelease: 6,
	OpTruncate: 3, OpDeallocate: 3, OpDelete: 1, OpSettle: 3, OpRepack: 2, OpCrash: 3, OpReopen: 1,
}

// recordHeader is the size of a record's header on disk (RFC 28 §2.3). A
// torn crash cuts the last unsynced write anywhere in its header, its
// write's 8-byte mtime or the first of its usually short payload, so the
// tear lands in each.
const recordHeader = 96

// DefaultSeed and DefaultSteps are the sequence the model-based test draws
// when no seed is given.
//
// ponytail: one sequence of 200 steps per run, comparing every file after
// every step, which keeps the run within a few seconds. Many seeds and longer
// sequences belong in the tiers that run for longer; raise DefaultSteps only
// if a failure needs a longer sequence to show and no corpus file holds it.
const (
	DefaultSeed  = 1
	DefaultSteps = 200
)

// Generate draws n steps from seed. The same seed draws the same steps.
//
// Extents are mostly small and near the start of the file, so writes overlap
// and split one another, and now and then far out, close to the largest
// offset. A write or a deallocate is now and then zero-length, and one
// truncate in four is to zero. One call in
// ten goes through the share that does not have the file.
func Generate(seed uint64, n int) []Step {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	total := 0
	for _, w := range weights {
		total += w
	}
	steps := make([]Step, n)
	for i := range steps {
		s := Step{File: r.IntN(numFiles), Byte: byte(r.Uint32()), Pick: byte(r.Uint32())}
		s.Share = s.File % numShares
		if r.IntN(10) == 0 {
			s.Share = 1 - s.Share
		}
		for w := r.IntN(total); ; s.Op++ {
			if w < weights[s.Op] {
				break
			}
			w -= weights[s.Op]
		}
		s.Off, s.Len = extentFor(r)
		if (s.Op == OpWrite || s.Op == OpDeallocate) && r.IntN(50) == 0 {
			s.Len = 0 // a zero-length call, which the journal may refuse
		}
		if s.Op == OpTruncate && r.IntN(4) == 0 {
			s.Off = 0 // a truncate to zero, after which the file is written again
		}
		if s.Op == OpCrash {
			s.Off = r.Int64N(recordHeader + 8 + 64) // the bytes a torn write keeps
			if k, _ := s.crash(); k == CrashReordered {
				s.Off = r.Int64N(math.MaxInt64) // the seed of the reordering
			}
			s.Len = 0
		}
		steps[i] = s
	}
	return steps
}

// extentFor draws an extent: mostly within the first 256 bytes and up to 64
// long, sometimes up to 1 KiB long, rarely at the end of the largest file.
func extentFor(r *rand.Rand) (off, n int64) {
	switch x := r.IntN(100); {
	case x < 2:
		n = 1 + r.Int64N(64)
		return math.MaxInt64 - 64 - n, n
	case x < 10:
		return r.Int64N(4096), 1 + r.Int64N(1024)
	default:
		return r.Int64N(256), 1 + r.Int64N(64)
	}
}
