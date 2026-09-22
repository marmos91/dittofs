package compression

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/middleware"
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
// Transform is the compression stage of a middleware pipeline. It compresses a
// chunk body on the way out and decompresses it on the way back in.
//
// Compression is per-block adaptive: if the compressed body is not strictly
// smaller than the plaintext, the stage emits the raw plaintext with no header.
// Open detects framed vs raw by the 5-byte DFCMP magic prefix, and passes an
// unframed body through unchanged.
//
// The plaintext BLAKE3 remains the CAS key — this stage never touches the hash,
// so dedup, GC and verification are unchanged above it.
type Transform struct {
	algo  Algo
	codec codec
}

// NewTransform builds the compression stage. The policy's algorithm is captured
// for the lifetime of the stage.
func NewTransform(p CompressionPolicy) (*Transform, error) {
	c, err := newCodec(p.Algo)
	if err != nil {
		return nil, err
	}
	return &Transform{algo: p.Algo, codec: c}, nil
}

// NewRemote wraps inner in a pipeline whose only stage is compression.
func NewRemote(inner remote.RemoteStore, p CompressionPolicy) (*middleware.Pipeline, error) {
	if inner == nil {
		return nil, fmt.Errorf("compression: inner RemoteStore is nil")
	}
	t, err := NewTransform(p)
	if err != nil {
		return nil, err
	}
	return middleware.New(inner, t)
}

// --- write path ---------------------------------------------------------

// sealLayer is the single source of this decorator's compression transform,
// shared by Put and SealChunk. It compresses data and returns the framed
// compressed body when that is strictly smaller than the input, otherwise the
// raw plaintext (incompressible blocks skip the frame).
func (d *Transform) Seal(_ context.Context, _ block.ContentHash, data []byte) ([]byte, error) {
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

func (d *Transform) Open(_ context.Context, _ block.ContentHash, raw []byte) ([]byte, error) {
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

// Compile-time interface assertion.
var _ middleware.Transform = (*Transform)(nil)
