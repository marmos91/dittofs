// Package carver cuts a contiguous byte stream into FastCDC chunks, hashes
// them, consults a per-chunk skip oracle, and accumulates the novel chunk bytes
// into block-sized batches. It wraps pkg/block/chunker — the chunker supplies
// the boundary search, the carver everything the journal's pack loop used to do
// inline around it.
//
// The two-call contract: feed each contiguous stream with Box, with final set
// on the stream's last call only — a chunk never spans the gap between two
// streams, but a block does. Once the caller's last stream is over, one Drain
// emits the trailing partial block; buffered chunk bytes never outlive the
// carver. A single-stream consumer is Box(..., true) then Drain().
package carver

import (
	"context"
	"crypto/subtle"
	"math"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block/chunker"
)

// Hash is a content hash of one chunk.
type Hash [32]byte

// Equal reports whether two hashes are the same content, in constant time.
func (h Hash) Equal(o Hash) bool { return subtle.ConstantTimeCompare(h[:], o[:]) == 1 }

// Chunk is one cut of the stream, in caller-data space: Offset is where the
// chunk lands in the caller's file, Size is its length, Hash its content. Data
// carries the chunk's bytes for novel chunks (the skip oracle said they still
// need uploading); skipped ones tile the range without carrying bytes.
type Chunk struct {
	Offset int64
	Size   int64
	Hash   Hash
	Data   []byte
}

// Block is one batch of novel chunks: the bytes the skip oracle said still need
// uploading, tiled by Chunks (deduped chunks included — they advance the tiling
// without carrying bytes).
type Block struct {
	Chunks []Chunk
	Bytes  int64
}

// Options configures a Carver. Params is the FastCDC sizing; BlockSize is the
// batch cap a block is emitted at; Skip answers per chunk whether its content
// is already durable remotely — novel chunks carry bytes, skipped ones only
// tile. An error from Skip stops the current call, matching the pack loop's
// first-error behaviour.
type Options struct {
	Params    chunker.Params
	BlockSize int64
	Skip      func(ctx context.Context, h Hash) (bool, error)
}

// Carver accumulates one file's novel chunk bytes into block-sized batches. It
// is not safe for concurrent use: one Carver per file per pass.
type Carver struct {
	opts      Options
	blockSize int64
	params    chunker.Params

	chunker *chunker.Chunker // fresh per stream, so a chunk never spans a gap
	buf     []byte           // chunker accumulator, never exceeds one max chunk

	pending    []Chunk // novel chunks of the batch being packed (skipped ones too)
	arena      []byte  // backing for the pending chunk copies, if any
	arenaOff   int     // fill cursor into arena
	batchBytes int64

	// The block currently being packed has not been emitted, so its Skip
	// answers must never observe it as durable: Skip consults only chunks the
	// caller has committed, never the carver's own pending batch.
	//
	// ponytail: the arena is one allocation per in-flight block, sized from the
	// caller's ChunkParams.Max rather than the package ceiling, so a share
	// chunking small does not reserve 16 MiB per slot. Sizing from the ceiling
	// also means the read loop cannot borrow the same constant, or it asks for
	// bytes past the arena's capacity.
}

// New returns a Carver configured with the given options. Invalid params fall
// back to the default profile because that is what the chunker itself does with
// them, so the arena matches the chunks actually cut; a non-positive BlockSize
// is likewise replaced, or nothing would ever emit.
func New(o Options) *Carver {
	if o.Params.Validate() != nil {
		o.Params = chunker.DefaultParams()
	}
	if o.BlockSize <= 0 {
		o.BlockSize = 1 << 20
	}
	c := &Carver{
		opts:      o,
		blockSize: o.BlockSize,
		params:    o.Params,
		buf:       make([]byte, 0, chunker.MaxChunkSize),
	}
	return c
}

// Box cuts data into chunks landing at off in the caller's file, and returns
// the blocks the batch reached BlockSize during this call, alongside how many
// bytes of stream this call tiled (riding bytes from previous calls included,
// below-Min riding bytes excluded) — the caller advances its file-offset cursor
// by that answer. final marks the end of the current contiguous stream: the
// below-Min tail is cut as a final chunk and the boundary search resets, so a
// chunk never spans the gap between two streams — but a block does, since the
// batch being packed carries over. final does not by itself emit the trailing
// partial block; Drain does that once the caller's last stream is over.
//
// Every byte of data is consumed by the time this returns (final=true), or
// buffered in the accumulator (final=false, below Min and more is coming).
// An error from Skip stops this call and returns the blocks cut so far; the
// first error is returned alongside them.
func (c *Carver) Box(ctx context.Context, data []byte, off int64, final bool) ([]Block, int64, error) {
	startOff := off
	if len(data) == 0 && len(c.buf) == 0 {
		return nil, 0, nil
	}
	// A fresh chunker per stream, and an empty accumulator, so a chunk never
	// spans the hole between two streams.
	if len(c.buf) == 0 {
		c.chunker = chunker.NewChunkerWithParams(c.params)
	}

	var (
		blocks []Block
		boxErr error
	)
	eof := final
	for {
		if err := ctx.Err(); err != nil {
			boxErr = err
			break
		}
		if len(data) > 0 {
			// Bound proof: buf never exceeds one max chunk, so the append below
			// cannot grow it past cap — the accumulator is sized at the
			// package-wide ceiling in New and the chunker never cuts longer
			// than its own ceiling.
			n := copy(c.buf[len(c.buf):cap(c.buf)], data)
			c.buf = c.buf[:len(c.buf)+n]
			data = data[n:]
		}
		if len(c.buf) == 0 {
			break
		}
		boundary, _ := c.chunker.Next(c.buf, eof)
		if boundary == 0 {
			if !eof {
				break // below Min and more is coming: caller feeds more
			}
			boundary = len(c.buf)
		}

		h := Hash(blake3.Sum256(c.buf[:boundary]))
		skip := false
		if c.opts.Skip != nil {
			s, err := c.opts.Skip(ctx, h)
			if err != nil {
				boxErr = err
				break
			}
			skip = s
		}
		ch := Chunk{Offset: off, Size: int64(boundary), Hash: h}
		if !skip {
			ch.Data = c.claim(c.buf[:boundary])
		}
		c.pending = append(c.pending, ch)
		c.batchBytes += int64(boundary)
		off += int64(boundary)
		c.buf = append(c.buf[:0], c.buf[boundary:]...)

		if c.batchBytes >= c.blockSize {
			blocks = append(blocks, c.emit())
		}
		if eof && len(c.buf) == 0 {
			break
		}
	}
	// off advanced by exactly the bytes this call tiled into chunks — the
	// caller advances its file-offset cursor by the same answer.
	return blocks, off - startOff, boxErr
}

// Drain emits the trailing partial block once the caller's last stream is over,
// flushing buffered chunk bytes so they never outlive the carver. An empty
// batch (fully deduped, or nothing new) returns nothing — a bare watermark.
func (c *Carver) Drain() []Block {
	if len(c.pending) == 0 {
		return nil
	}
	return []Block{c.emit()}
}

// emit returns the batch being packed as one block and resets the batch state.
// The arena's backing moves to the block's chunks (still live while the caller
// commits), so the local state resets to "no batch".
func (c *Carver) emit() Block {
	// Bytes reports only what needs uploading: skipped chunks tile the range
	// without carrying bytes, so they cost the caller nothing to commit.
	var novel int64
	for _, ch := range c.pending {
		if ch.Data != nil {
			novel += ch.Size
		}
	}
	b := Block{Chunks: c.pending, Bytes: novel}
	c.pending, c.arena, c.arenaOff, c.batchBytes = nil, nil, 0, 0
	return b
}

// claim copies one novel chunk's bytes into the arena backing the pending
// batch, returning the data slice that ships with the chunk. The arena is
// preallocated to one block plus one overhang chunk in New; the grow is a
// fail-loud belt if that invariant ever breaks.
func (c *Carver) claim(data []byte) []byte {
	if c.arena == nil {
		c.arena = make([]byte, 0, c.arenaCap())
	}
	if c.arenaOff+len(data) > cap(c.arena) {
		// Already-pending slices keep pointing at the old backing (still
		// live), so no copy is needed — the new chunk lands in the larger
		// arena.
		c.arena = make([]byte, c.arenaOff+len(data))
	}
	c.arena = c.arena[:c.arenaOff+len(data)]
	copy(c.arena[c.arenaOff:], data)
	c.arenaOff += len(data)
	return c.arena[c.arenaOff-len(data) : c.arenaOff : c.arenaOff]
}

// arenaCap sizes the arena backing one in-flight block: one block plus the
// overhang chunk that crossed the line. A block is emitted once it reaches
// BlockSize, so it overshoots by at most one chunk, and no chunk exceeds
// Params.Max.
//
// Clamp the block size before adding the overhang, not after: a pathological
// BlockSize near the int64 ceiling would wrap to a negative sum and reach
// make() as a negative length. BlockSize is positive (New replaces anything
// <= 0) and the overhang is at most chunker.MaxChunkSize, so the subtraction
// below cannot itself go negative.
func (c *Carver) arenaCap() int {
	overhang := c.params.Max
	overhang64 := int64(overhang)
	blockCap64 := c.blockSize
	if blockCap64 > math.MaxInt-overhang64 {
		blockCap64 = math.MaxInt - overhang64
	}
	return int(blockCap64 + overhang64)
}
