package model

import (
	"fmt"
	"math"
)

// Op is the operation a step of the model-based test calls.
type Op uint8

// The operations a step calls. Each names the journal call the runner makes
// and the model operation it applies the answer to.
const (
	// OpWrite writes Len bytes, made from Byte, at Off.
	OpWrite Op = iota + 1
	// OpSync syncs the file.
	OpSync
	// OpRead reads the file and keeps the change version the read reports,
	// which the next fill of the file is made as of.
	OpRead
	// OpFill fills one extent an offload of the file reported, at the version
	// it was offered at, as of the change version the last read of the file
	// reported. When nothing was offloaded from the file, it fills the next
	// file something was, through that file's share, or through the other
	// share when the step names the share that does not have its own file.
	OpFill
	// OpStaleFill is OpFill as of the change version of the read before the
	// last, so a change between the two makes the fill stale.
	OpStaleFill
	// OpOffload offloads the file through every version issued. Pick chooses
	// what the callback reports: everything offered, nothing, part of it, or
	// an extent it was not offered. Its low bit set, the callback first
	// writes Len bytes at Off, so a write lands during the offer. Its higher
	// bits choose the offer's limit and widen, each none, small or larger.
	OpOffload
	// OpRelease releases one extent an offload reported, or Off and Len
	// when none did.
	OpRelease
	// OpTruncate truncates the file to Off.
	OpTruncate
	// OpDeallocate deallocates Len bytes at Off.
	OpDeallocate
	// OpDelete deletes the file.
	OpDelete
	// OpSettle settles the file's removals through a version Pick chooses,
	// as a share of every version issued.
	OpSettle
	// OpRepack runs a repack pass over the whole journal.
	OpRepack
	// OpCrash loses the journal's storage and reopens it. Pick chooses the
	// crash: unsynced writes lost, the last one torn after Off bytes, or a
	// subset of them kept, chosen from Off.
	OpCrash
	// OpReopen closes the journal cleanly and opens it again.
	OpReopen

	opEnd
)

var opNames = [...]string{
	OpWrite: "write", OpSync: "sync", OpRead: "read", OpFill: "fill", OpStaleFill: "stalefill",
	OpOffload: "offload", OpRelease: "release", OpTruncate: "truncate", OpDeallocate: "deallocate",
	OpDelete: "delete", OpSettle: "settle", OpRepack: "repack", OpCrash: "crash", OpReopen: "reopen",
}

func (o Op) String() string {
	if o == 0 || o >= opEnd {
		return fmt.Sprintf("op(%d)", uint8(o))
	}
	return opNames[o]
}

// The shares and files a sequence reaches: two shares on one journal, and
// files enough that each share has two of its own on the journal's streams.
const (
	numShares = 2
	numFiles  = 4
)

// CrashKind is the storage loss of a crash step.
type CrashKind uint8

// The three crashes the storage seam simulates.
const (
	// CrashLost loses every write no sync covered.
	CrashLost CrashKind = iota
	// CrashTorn keeps the last unsynced write cut to its first n bytes.
	CrashTorn
	// CrashReordered keeps a subset of the unsynced writes, chosen from a
	// seed, as a device writing them out of order would.
	CrashReordered
)

func (k CrashKind) String() string {
	if k > CrashReordered {
		return fmt.Sprintf("crash(%d)", uint8(k))
	}
	return [...]string{"lost", "torn", "reordered"}[k]
}

// Step is one call of the model-based test. It names a share and a file by
// number, and holds only small values: every choice that depends on what ran
// before, such as which offloaded extent a fill takes or which version a
// settle reaches, is resolved when the step runs. A sequence with steps
// removed therefore still runs, which is what lets a failing one be shrunk.
//
// decision: a step whose choice finds nothing to take, such as a fill before
// anything was offloaded, does nothing. Shrinking removes the steps a choice
// depended on, and a step that failed instead would turn every such removal
// into a different failure. Check logs how many fill, crash and reopen steps
// did nothing, so a sequence that reads as passing because its choices found
// nothing shows it; draw offloads earlier or more often if fewer than ten
// fills of the default sequence fill.
type Step struct {
	Op    Op
	Share int // 0 or 1
	File  int // 0 to numFiles-1
	Off   int64
	Len   int64
	Byte  byte  // the bytes written, and a write's mtime
	Pick  uint8 // the choice the step resolves when it runs
}

// valid returns why s cannot be run, or "" when it can.
func (s Step) valid() string {
	switch {
	case s.Op == 0 || s.Op >= opEnd:
		return fmt.Sprintf("no operation %d", s.Op)
	case s.Share < 0 || s.Share >= numShares:
		return fmt.Sprintf("no share %d", s.Share)
	case s.File < 0 || s.File >= numFiles:
		return fmt.Sprintf("no file %d", s.File)
	case s.Off < 0 || s.Len < 0:
		return "a negative offset or length"
	case s.Len > math.MaxInt64-s.Off:
		return "an extent past the largest offset"
	}
	return ""
}

// The limits and widens an offload step chooses from, by its pick: none half
// the time, so most offers carry everything dirty, and otherwise a limit
// that cuts an extent to one byte or a few, and a widen within or past the
// short extents a sequence writes.
var (
	offerLimits = [...]int64{0, 0, 1, 16}
	offerWidens = [...]int64{0, 0, 8, 64}
)

// offer returns the limit and widen of an offload step.
func (s Step) offer() (limit, widen int64) {
	return offerLimits[s.Pick>>3%4], offerWidens[s.Pick>>5%4]
}

// crash returns the crash a crash step simulates, and its argument: the bytes
// a torn write keeps, or the seed of a reordering.
func (s Step) crash() (CrashKind, uint64) {
	return CrashKind(s.Pick % 3), uint64(s.Off)
}
