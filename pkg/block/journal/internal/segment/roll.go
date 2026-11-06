package segment

import (
	"fmt"
	"math"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// Streams is the journal's append streams: Len of them, the stream of a file
// chosen by a hash of its FileID. All of them share one ledger, one spare
// pool and one descriptor cache, each keeping its own active segment and
// sealing and rolling on its own.
//
// decision: the slice needs no lock domain because it is never written
// after NewStreams builds it: the stream count is fixed at construction, and
// every later access reads the same pointers. Withdrawn the day a stream is
// ever added or removed past the build, which would need a domain of its own
// over the slice.
type Streams struct {
	s []*Stream
}

// NewStreams returns n append streams over the ledger set, taking their
// segments from the pool and reading and writing them through cache. It
// panics on a nil dependency, as NewStream does, and on a count below one or
// above MaxUint32, for no header could carry a stream number of the kind. Every
// stream shares the set, the pool and the cache, so a file's roll and every
// other's draw on the one spare pool and the one footprint. The sink is built
// once per stream, by calling sink with the stream's number, so a consumer
// registers functions that reach the one stream's own state and no other's. It
// panics on a nil sink function.
func NewStreams(n int, identity core.JournalID, set *Set, pool *Pool, cache *Descriptors, sink func(no uint32) Sink) *Streams {
	if n < 1 || uint64(n) > math.MaxUint32 {
		// Below 1 and above the 32-bit column the header carries are one
		// refusal: no stream number the format spells could name a stream
		// of that count, and on a 32-bit int the plain upper comparison
		// would overflow before it refused.
		panic(fmt.Sprintf("segment: the stream count %d is below 1 or above MaxUint32, so no header could carry its stream numbers", n))
	}
	if sink == nil {
		panic("segment: the streams need a function that builds each one's durability sink")
	}
	ss := &Streams{s: make([]*Stream, n)}
	for i := range ss.s {
		ss.s[i] = NewStream(uint32(i), identity, set, pool, cache, sink(uint32(i)))
	}
	return ss
}

// For returns the stream the file's appends belong to: the hash of its
// FileID over the streams. A file's records stay ordered on the one stream,
// so the file's append serialisation needs no cross-stream order.
//
// ponytail: the hash chooses one stream per file, so one hot file cannot use
// more than one stream while others idle. Upgrade to a per-file stream
// choice that spreads a hot file, once a profile shows one stream saturated
// while others sit idle.
func (ss *Streams) For(file core.FileID) *Stream {
	// FNV-1a 64 over the file's identifier, written inline: no allocation,
	// deterministic across processes, and the offset basis and the prime
	// are the function's own.
	id := file.Bytes()
	h := uint64(14695981039346656037)
	for _, b := range id {
		h ^= uint64(b)
		h *= 1099511628211
	}
	return ss.s[h%uint64(len(ss.s))]
}

// Stream returns stream i, for the callers that walk the streams rather than
// a file's own: 0 <= i < Len, and no other.
func (ss *Streams) Stream(i int) *Stream {
	return ss.s[i]
}

// Len returns how many streams the journal appends over.
func (ss *Streams) Len() int {
	return len(ss.s)
}

// OnEnd registers fn as the consumer of every stream's end events: each stream
// hands its own over, so fn can see events of any of them. It must be called
// before the first append of any stream, and panics past one, as Stream.OnEnd
// does.
func (ss *Streams) OnEnd(fn func(EndEvent)) {
	for _, s := range ss.s {
		s.OnEnd(fn)
	}
}

// Close closes every stream. It answers nil today, for a stream's close has no
// failure of its own; the error stays in the signature for the group's close
// to join.
func (ss *Streams) Close() error {
	// Two phases, so the set is never half closed: every stream stops
	// admitting appends and reads first, then each is drained.
	type begun struct {
		ref   Ref
		began bool
	}
	b := make([]begun, len(ss.s))
	for i, s := range ss.s {
		b[i].ref, b[i].began = s.beginClose()
	}
	for i, s := range ss.s {
		if b[i].began {
			s.finishClose(b[i].ref)
		}
	}
	return nil
}
