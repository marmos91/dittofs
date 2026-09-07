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
	_ remote.RemoteStore       = (*Decorator)(nil)
	_ remote.RemoteBlockStore  = (*Decorator)(nil)
	_ remote.ChunkReader       = (*Decorator)(nil)
	_ remote.ChunkSealer       = (*Decorator)(nil)
	_ block.DurabilityReporter = (*Decorator)(nil)
)
