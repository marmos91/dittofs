package compression

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/remote"
)

// Decorator wraps a remote.RemoteStore and transparently compresses
// block bodies on Put while decompressing on Get. The plaintext BLAKE3
// remains the CAS key — dedup, GC, and verification semantics are
// unchanged from the perspective of callers above the decorator.
//
// Compression is per-block adaptive: if the compressed body is not
// strictly smaller than the plaintext, the decorator stores the raw
// plaintext with no header. Get detects framed vs raw by checking the
// 5-byte DFCMP magic prefix.
type Decorator struct {
	remote.Passthrough
	// inner is the wrapped store, held separately because Passthrough keeps
	// its own copy unexported to stay off this type's public surface.
	inner remote.RemoteStore
	algo  Algo
	codec codec
}

// NewRemote constructs a compression decorator wrapping inner. The
// policy's algorithm is captured for the lifetime of the decorator.
func NewRemote(inner remote.RemoteStore, p CompressionPolicy) (*Decorator, error) {
	if inner == nil {
		return nil, fmt.Errorf("compression: inner RemoteStore is nil")
	}
	c, err := newCodec(p.Algo)
	if err != nil {
		return nil, err
	}
	return &Decorator{
		Passthrough: remote.NewPassthrough(inner),
		inner:       inner,
		algo:        p.Algo,
		codec:       c,
	}, nil
}

// --- write path ---------------------------------------------------------

// Put compresses data; if the result is strictly smaller than the input
// (header overhead included) it stores the framed compressed body, else
// it stores the raw plaintext with no header — incompressible blocks
// skip the allocate-and-copy frame build entirely.
//
// Put and SealChunk share the same single-layer transform (sealLayer) so the
// standalone-object and packed-block write paths never drift.
func (d *Decorator) Put(ctx context.Context, hash block.ContentHash, data []byte) error {
	wire, err := d.sealLayer(data)
	if err != nil {
		return err
	}
	cs, err := remote.CASInner(d.inner)
	if err != nil {
		return err
	}
	return cs.Put(ctx, hash, wire)
}

// SealChunk applies this decorator's compression layer to plaintext, then
// delegates to the inner store's ChunkSealer so a decorated chain produces the
// fully-transformed wire bytes for a packed block. Implements
// remote.ChunkSealer (#1414). Symmetric with ReadChunk: ReadChunk decompresses
// after the inner layer decrypts, inverting this exactly.
func (d *Decorator) SealChunk(ctx context.Context, hash block.ContentHash, plaintext []byte) ([]byte, error) {
	wire, err := d.sealLayer(plaintext)
	if err != nil {
		return nil, err
	}
	sealer, ok := d.inner.(remote.ChunkSealer)
	if !ok {
		return nil, remote.ErrChunkReadUnsupported
	}
	return sealer.SealChunk(ctx, hash, wire)
}

// sealLayer is the single source of this decorator's compression transform,
// shared by Put and SealChunk. It compresses data and returns the framed
// compressed body when that is strictly smaller than the input, otherwise the
// raw plaintext (incompressible blocks skip the frame).
func (d *Decorator) sealLayer(data []byte) ([]byte, error) {
	// Reserve the frame header up front and let the codec stream the compressed
	// body straight after it, so the buffer already holds the wire form when the
	// frame wins — no second allocate-and-copy of the whole body.
	header := appendFrameHeader(make([]byte, 0, FrameHeaderFixedSize+maxOrigSizeVarint), d.algo, uint64(len(data)))
	framed := bytes.NewBuffer(header)
	enc, err := d.codec.EncodeStream(framed)
	if err != nil {
		return nil, fmt.Errorf("compression: EncodeStream: %w", err)
	}
	if _, err := enc.Write(data); err != nil {
		_ = enc.Close()
		return nil, fmt.Errorf("compression: encoder write: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("compression: encoder close: %w", err)
	}

	// framed.Len() is header + body, the byte count the frame would put on the
	// wire; incompressible blocks skip the frame and travel as plaintext.
	if framed.Len() < len(data) {
		return framed.Bytes(), nil
	}
	return data, nil
}

// --- read path ----------------------------------------------------------

// Get returns the plaintext for the block identified by hash.
func (d *Decorator) Get(ctx context.Context, hash block.ContentHash) ([]byte, error) {
	cs, err := remote.CASInner(d.inner)
	if err != nil {
		return nil, err
	}
	raw, err := cs.Get(ctx, hash)
	if err != nil {
		return nil, err
	}
	return d.decode(raw)
}

func (d *Decorator) decode(raw []byte) ([]byte, error) {
	algo, origSize, body, framed, err := tryDecodeFrame(raw)
	if !framed {
		return raw, nil
	}
	if err != nil {
		return nil, err
	}
	if origSize > MaxFramedPlaintextSize {
		return nil, fmt.Errorf("%w: declared plaintext size %d exceeds cap %d", ErrCompressedFrameCorrupt, origSize, MaxFramedPlaintextSize)
	}
	c, err := newCodec(algo)
	if err != nil {
		return nil, err
	}
	dec, err := c.DecodeStream(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("compression: DecodeStream: %w", err)
	}
	defer func() { _ = dec.Close() }()

	// Cap the read at origSize+1: anything over origSize means the body
	// produced more bytes than the header declared (corrupt frame). The
	// +1 lets the truncation check below trip on overflow rather than
	// silently succeed when the codec emits extra bytes.
	limited := io.LimitReader(dec, int64(origSize)+1)
	buf := bytes.NewBuffer(make([]byte, 0, int(origSize)))
	if _, err := io.Copy(buf, limited); err != nil {
		return nil, fmt.Errorf("compression: decode: %w", err)
	}
	out := buf.Bytes()
	if uint64(len(out)) != origSize {
		return nil, fmt.Errorf("%w: decoded %d bytes, header declared %d", ErrCompressedFrameCorrupt, len(out), origSize)
	}
	return out, nil
}

// GetRange returns a byte sub-range of the plaintext. For framed
// blocks this materialises the full plaintext and slices — there is no
// random access into compressed bodies.
func (d *Decorator) GetRange(ctx context.Context, hash block.ContentHash, offset, length int64) ([]byte, error) {
	if length <= 0 {
		return nil, fmt.Errorf("%w: length %d", block.ErrInvalidSize, length)
	}
	full, err := d.Get(ctx, hash)
	if err != nil {
		return nil, err
	}
	return remote.SliceRange(full, offset, length)
}

// Head returns Meta whose Size is the plaintext byte length. For
// framed blocks this requires a short range-GET to parse the frame
// header.
func (d *Decorator) Head(ctx context.Context, hash block.ContentHash) (block.Meta, error) {
	cs, err := remote.CASInner(d.inner)
	if err != nil {
		return block.Meta{}, err
	}
	m, err := cs.Head(ctx, hash)
	if err != nil {
		return m, err
	}
	size, err := d.plaintextSizeFor(ctx, hash, m.Size)
	if err != nil {
		return block.Meta{}, err
	}
	m.Size = size
	return m, nil
}

// plaintextSizeFor returns the plaintext byte length of the block by
// probing the frame header in the inner store. For framed blocks the
// header carries the plaintext size; for raw passthrough blocks
// wireSize already equals plaintext size and is returned unchanged.
//
// A probe failure is propagated as an error rather than silently
// reporting wireSize: when the magic check can't run, the caller has
// no way to distinguish a raw block (wireSize correct) from a framed
// block (wireSize is the compressed size) — surfacing the error keeps
// Meta.Size honest per the blockstore.go:130 contract.
func (d *Decorator) plaintextSizeFor(ctx context.Context, hash block.ContentHash, wireSize int64) (int64, error) {
	probeLen := min(int64(FrameHeaderFixedSize+maxOrigSizeVarint), wireSize)
	if probeLen <= 0 {
		return wireSize, nil
	}
	cs, err := remote.CASInner(d.inner)
	if err != nil {
		return 0, err
	}
	probe, err := cs.GetRange(ctx, hash, 0, probeLen)
	if err != nil {
		return 0, fmt.Errorf("compression: plaintext-size probe: %w", err)
	}
	_, origSize, _, framed, err := tryDecodeFrame(probe)
	if err != nil {
		return 0, err
	}
	if !framed {
		return wireSize, nil
	}
	return int64(origSize), nil
}

// Walk wraps the inner Walk and rewrites Meta.Size to plaintext size
// for each framed block before invoking the user callback. Per-block
// probe errors halt the walk and are surfaced to the caller.
func (d *Decorator) Walk(ctx context.Context, fn func(hash block.ContentHash, meta block.Meta) error) error {
	cs, err := remote.CASInner(d.inner)
	if err != nil {
		return err
	}
	return cs.Walk(ctx, func(h block.ContentHash, m block.Meta) error {
		size, err := d.plaintextSizeFor(ctx, h, m.Size)
		if err != nil {
			return err
		}
		m.Size = size
		return fn(h, m)
	})
}

// ReadChunk reads the chunk's compressed wire bytes from the inner store's
// block object and decompresses them, returning the next layer's input (or the
// engine's plaintext). A block stores each chunk's full self-framed compression
// blob (or raw passthrough) verbatim, so decoding the chunk's [offset, length)
// slice is identical to decoding its standalone object. No verification here —
// the engine verifies the BLAKE3 after the full stack. hash is unused at this
// layer. Implements remote.ChunkReader (#1414).
func (d *Decorator) ReadChunk(ctx context.Context, blockID string, offset, length int64, hash block.ContentHash) ([]byte, error) {
	pcr, ok := d.inner.(remote.ChunkReader)
	if !ok {
		return nil, remote.ErrChunkReadUnsupported
	}
	raw, err := pcr.ReadChunk(ctx, blockID, offset, length, hash)
	if err != nil {
		return nil, err
	}
	return d.decode(raw)
}

// Compile-time interface assertions.
var (
	_ block.Store              = (*Decorator)(nil)
	_ remote.RemoteStore       = (*Decorator)(nil)
	_ remote.RemoteBlockStore  = (*Decorator)(nil)
	_ remote.ChunkReader       = (*Decorator)(nil)
	_ remote.ChunkSealer       = (*Decorator)(nil)
	_ block.DurabilityReporter = (*Decorator)(nil)
)
