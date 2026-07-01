package compression

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/remote"
	"github.com/marmos91/dittofs/pkg/health"
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
	return &Decorator{inner: inner, algo: p.Algo, codec: c}, nil
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
	return d.inner.Put(ctx, hash, wire)
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
	var compressed bytes.Buffer
	enc, err := d.codec.EncodeStream(&compressed)
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

	wire := data
	body := compressed.Bytes()
	origSize := uint64(len(data))
	if frameOverhead(origSize)+len(body) < len(data) {
		wire = encodeFrame(d.algo, origSize, body)
	}
	return wire, nil
}

// --- read path ----------------------------------------------------------

// Get returns the plaintext for the block identified by hash.
func (d *Decorator) Get(ctx context.Context, hash block.ContentHash) ([]byte, error) {
	raw, err := d.inner.Get(ctx, hash)
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
	if offset < 0 || offset > int64(len(full)) {
		return nil, fmt.Errorf("%w: offset %d out of bounds (size %d)", block.ErrInvalidOffset, offset, len(full))
	}
	end := min(offset+length, int64(len(full)))
	out := make([]byte, end-offset)
	copy(out, full[offset:end])
	return out, nil
}

// Has reports presence by probing inner.Head. NotFound errors map to
// (false, nil); any other backend error propagates.
func (d *Decorator) Has(ctx context.Context, hash block.ContentHash) (bool, error) {
	_, err := d.inner.Head(ctx, hash)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, block.ErrChunkNotFound) {
		return false, nil
	}
	return false, err
}

// Head returns Meta whose Size is the plaintext byte length. For
// framed blocks this requires a short range-GET to parse the frame
// header.
func (d *Decorator) Head(ctx context.Context, hash block.ContentHash) (block.Meta, error) {
	m, err := d.inner.Head(ctx, hash)
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
	probe, err := d.inner.GetRange(ctx, hash, 0, probeLen)
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
	return d.inner.Walk(ctx, func(h block.ContentHash, m block.Meta) error {
		size, err := d.plaintextSizeFor(ctx, h, m.Size)
		if err != nil {
			return err
		}
		m.Size = size
		return fn(h, m)
	})
}

// Delete is a straight passthrough.
func (d *Decorator) Delete(ctx context.Context, hash block.ContentHash) error {
	return d.inner.Delete(ctx, hash)
}

// --- remote.RemoteStore extras -----------------------------------------

// ReadBlockVerified GETs the block, decodes it, then re-verifies the
// BLAKE3 hash over the plaintext. Streaming verification over the wire
// bytes is not possible once the body is compressed.
func (d *Decorator) ReadBlockVerified(ctx context.Context, hash block.ContentHash, expected block.ContentHash) ([]byte, error) {
	plain, err := d.Get(ctx, hash)
	if err != nil {
		return nil, err
	}
	actual := blake3.Sum256(plain)
	var got block.ContentHash
	copy(got[:], actual[:])
	if got != expected {
		return nil, fmt.Errorf("%w: got %s want %s", block.ErrCASContentMismatch, got, expected)
	}
	return plain, nil
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

// Close releases inner resources.
func (d *Decorator) Close() error { return d.inner.Close() }

// HealthCheck delegates to inner.
func (d *Decorator) HealthCheck(ctx context.Context) error { return d.inner.HealthCheck(ctx) }

// Healthcheck delegates to inner.
func (d *Decorator) Healthcheck(ctx context.Context) health.Report {
	return d.inner.Healthcheck(ctx)
}

// Durable delegates to the wrapped store via block.IsDurable. Compressing block
// bodies does not change where the bytes ultimately land, so a durable inner
// store stays durable through the decorator; a wrapped store that does not
// report durability falls back to the conservative default (false).
func (d *Decorator) Durable() bool {
	return block.IsDurable(d.inner)
}

// --- remote.RemoteBlockStore passthrough (#1414) ---
//
// Packed block objects carry per-chunk wire bodies that were already sealed via
// SealChunk; the assembled block must NOT be re-transformed at the block level.
// So every block-keyed operation passes through verbatim to the inner store.
// This lets a decorated remote satisfy remote.RemoteBlockStore (the carve path
// asserts it) while the per-chunk transform stays in SealChunk/ReadChunk.

// blockInner returns the inner store as a remote.RemoteBlockStore, or an error
// when the wrapped store does not support block-keyed objects.
func (d *Decorator) blockInner() (remote.RemoteBlockStore, error) {
	rbs, ok := d.inner.(remote.RemoteBlockStore)
	if !ok {
		return nil, remote.ErrChunkReadUnsupported
	}
	return rbs, nil
}

// PutBlock stores the assembled block verbatim (already-sealed bodies).
func (d *Decorator) PutBlock(ctx context.Context, blockID string, r io.Reader) error {
	rbs, err := d.blockInner()
	if err != nil {
		return err
	}
	return rbs.PutBlock(ctx, blockID, r)
}

// GetBlock returns the raw block object verbatim.
func (d *Decorator) GetBlock(ctx context.Context, blockID string) ([]byte, error) {
	rbs, err := d.blockInner()
	if err != nil {
		return nil, err
	}
	return rbs.GetBlock(ctx, blockID)
}

// GetBlockRange returns raw block bytes verbatim; per-chunk decode is ReadChunk.
func (d *Decorator) GetBlockRange(ctx context.Context, blockID string, offset, length int64) ([]byte, error) {
	rbs, err := d.blockInner()
	if err != nil {
		return nil, err
	}
	return rbs.GetBlockRange(ctx, blockID, offset, length)
}

// DeleteBlock removes the block object.
func (d *Decorator) DeleteBlock(ctx context.Context, blockID string) error {
	rbs, err := d.blockInner()
	if err != nil {
		return err
	}
	return rbs.DeleteBlock(ctx, blockID)
}

// WalkBlocks enumerates block objects.
func (d *Decorator) WalkBlocks(ctx context.Context, fn func(blockID string, meta block.Meta) error) error {
	rbs, err := d.blockInner()
	if err != nil {
		return err
	}
	return rbs.WalkBlocks(ctx, fn)
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
