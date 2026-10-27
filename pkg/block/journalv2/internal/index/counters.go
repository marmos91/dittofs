package index

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"sync/atomic"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// Numbers is the pair of numbers a record carries: its content version and its
// sequence number. Where two records cover the same byte, the one whose pair
// is the greater wins, compared version first.
type Numbers struct {
	Version core.Version
	Seq     core.Seq
}

// Compare returns -1 if a is outranked by b, 0 if the pairs are equal and +1
// if a outranks b: the higher version wins, and of two equal versions the
// higher sequence number.
func (a Numbers) Compare(b Numbers) int {
	if c := a.Version.Compare(b.Version); c != 0 {
		return c
	}
	return cmp.Compare(a.Seq, b.Seq)
}

// ErrSeqExhausted refuses a sequence number once the counter stands at the top
// of its range, where the next one would wrap to zero and be outranked by
// every record already appended. NextSeq returns no other error.
//
// decision: an index error, not one of the journal's. Issuing or observing a
// number at the top of the range brings the counter here: no journal issues
// 2^64 numbers, so in practice only a record from another writer or from
// damage does, and it is left to the code that wires the counters in to decide
// how the journal reports it.
var ErrSeqExhausted = errors.New("index: sequence numbers exhausted")

// The greatest numbers the counters issue. The greatest version is one below
// the greatest counter value, since the version limit is exclusive and never
// above it; sequence numbers stop at the same value so the two read alike.
//
// decision: a record above these is refused when it arrives, as an unknown
// format: it carries a number this journal never issues, so it comes from
// another writer or from damage, and failing the open that meets it is louder
// than any later refusal. A floor above the greatest version is clamped to it
// instead, because the attach rule feeds a share's durable version limit back
// as the floor, and SetVersionLimit accepts a limit at the greatest counter
// value, which a caller may pass for "no limit": refusing it would fail the
// attach and leave that share's offloaded content unreadable, while a clamped
// counter refuses only draws, which is the true state once every version below
// the top may have been issued. Everything the counters issue stays at or
// below them, so a journal always reopens on its own records and on floors
// taken from its own limits. The format allows the values above, so the
// refusal is narrower than the format; widen it, and the issuing caps with it,
// if another writer can legitimately carry them.
const (
	maxVersion = math.MaxUint64 - 1
	maxSeq     = math.MaxUint64 - 1
)

// Counters issues the journal's content versions and sequence numbers: one
// counter of each per journal, never per file or per segment. No number of
// either kind is issued twice, and every version issued exceeds every version
// observed on disk and every floor raised to. Versions are issued only below
// the version limit, and none at all until a limit is set.
//
// The zero value issues no version, until SetVersionLimit, and issues
// sequence numbers from 1.
//
// In this format a version's epoch half is zero, so only counter halves are
// kept: a floor or a record with a non-zero epoch is refused, and a limit with
// one stands above every version the counter can issue.
//
// decision: the counters are atomics with compare-and-swap, not one mutex.
// Every lock the journal takes belongs to a named domain in one total order,
// and a mutex here would add a store-wide domain to that order for every
// append of every file, while the atomics add none. The swap also makes the
// limit check and the increment one step, so concurrent draws can never pass
// the limit between them.
//
// decision: the swap guards the counter, not the limit, so a draw in flight
// that read the old limit can still issue one version at or above a limit set
// since, though below the old one. That window is unreachable under the attach
// rule, which writes a share's limit as a tracked max-write that never lowers
// it and writes an attaching share's limit at the journal's, so the journal's
// limit never falls; setting rather than raising is the safe direction all the
// same. Take a lock that covers the limit too if a caller ever sets a limit
// below the current one.
type Counters struct {
	high    atomic.Uint64 // the highest version issued, observed or raised to
	limit   atomic.Uint64 // versions are issued only below it; zero issues none
	seqHigh atomic.Uint64 // the highest sequence number issued or observed
}

// Draw issues the next content version and records nothing else. At the
// version limit, or before one is set, it is refused with a NoSpaceError
// naming LimitVersion and issues nothing. A counter at the top of its range,
// reached by issuing or by a floor or record there, is refused the same way.
func (c *Counters) Draw() (core.Version, error) {
	for {
		high, limit := c.high.Load(), c.limit.Load()
		// The counter never stands above one below the top of its range, so
		// high+1 never wraps, and a zero limit refuses every draw.
		if high+1 >= limit {
			return core.Version{}, &core.NoSpaceError{Limit: core.LimitVersion, Missing: 1, Unit: core.UnitVersions}
		}
		if c.high.CompareAndSwap(high, high+1) {
			return core.Version{Counter: high + 1}, nil
		}
	}
}

// NextSeq issues the next sequence number, or refuses with ErrSeqExhausted
// once the counter stands at the top of its range.
func (c *Counters) NextSeq() (core.Seq, error) {
	for {
		high := c.seqHigh.Load()
		if high >= maxSeq {
			return 0, ErrSeqExhausted
		}
		if c.seqHigh.CompareAndSwap(high, high+1) {
			return core.Seq(high + 1), nil
		}
	}
}

// SetVersionLimit sets the version limit: versions are issued only below
// mark, and none at all until one is set. It sets rather than raises, so a
// limit below the current one takes effect at once.
//
// decision: a mark with a non-zero epoch is accepted, unlike a floor or a
// record, and kept as the top of the counter's range. A mark is the caller's
// durable bound, not a version this format stores, and every version the
// counter can issue lies below one with a non-zero epoch, so taking it as the
// top leaves nothing issued that the mark forbids. The mark comes back as the
// floor at the next open or attach: a counter half is clamped to the greatest
// version issued, and a non-zero epoch is refused there as an unknown format.
// Refuse such a mark here instead if a later format issues versions with a
// non-zero epoch, where the mark's counter half would be its limit.
func (c *Counters) SetVersionLimit(mark core.Version) {
	limit := mark.Counter
	if mark.Epoch != 0 {
		limit = math.MaxUint64
	}
	c.limit.Store(limit)
}

// RaiseFloor raises the version counter so that every version issued after it
// returns exceeds floor: the floor given at open, or a share's own floor when
// it attaches. A lower floor leaves the counter where it is, and one above
// every version this journal issues is clamped to the greatest, where draws
// are refused. A floor with a non-zero epoch is refused with ErrUnknownFormat
// and changes nothing: at open, which it fails, and at a share's attach, which
// it fails the same way.
func (c *Counters) RaiseFloor(floor core.Version) error {
	if floor.Epoch != 0 {
		return fmt.Errorf("%w: version floor %v has a non-zero epoch", core.ErrUnknownFormat, floor)
	}
	raise(&c.high, min(floor.Counter, maxVersion))
	return nil
}

// Observe raises both counters above the numbers of a record found on disk,
// so that neither issues a number the record carries. Records may be observed
// in any order, and concurrently. A record whose version has a non-zero
// epoch, or which carries a number above every one this journal issues, is
// refused with ErrUnknownFormat and changes neither counter.
func (c *Counters) Observe(n Numbers) error {
	if n.Version.Epoch != 0 {
		return fmt.Errorf("%w: record version %v has a non-zero epoch", core.ErrUnknownFormat, n.Version)
	}
	if n.Version.Counter > maxVersion || n.Seq > maxSeq {
		return fmt.Errorf("%w: record version %v, seq %d, is above every number issued", core.ErrUnknownFormat, n.Version, n.Seq)
	}
	raise(&c.high, n.Version.Counter)
	raise(&c.seqHigh, uint64(n.Seq))
	return nil
}

// NextVersion returns the version the next Draw issues if the limit allows it.
// The counter never passes one below the top of its range, so this never
// wraps. Once the counter stands there, the version returned is one no limit
// allows and the counter never issues.
func (c *Counters) NextVersion() core.Version {
	return core.Version{Counter: c.high.Load() + 1}
}

// VersionsLeft returns how many versions remain to be issued below the limit:
// zero when none is set, or the next version would be at or above it.
func (c *Counters) VersionsLeft() uint64 {
	high, limit := c.high.Load(), c.limit.Load()
	if high+1 >= limit {
		return 0
	}
	return limit - (high + 1)
}

// raise sets a to x if x is greater, and leaves it otherwise.
func raise(a *atomic.Uint64, x uint64) {
	for {
		cur := a.Load()
		if x <= cur || a.CompareAndSwap(cur, x) {
			return
		}
	}
}
