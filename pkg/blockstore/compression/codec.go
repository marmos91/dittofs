package compression

import (
	"fmt"
	"io"
	"sync"

	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
)

// codec is the streaming interface every compression algorithm
// implementation satisfies. EncodeStream wraps a writer that receives
// compressed bytes; the caller writes plaintext into the returned
// WriteCloser and MUST call Close to flush + release the pooled
// encoder. DecodeStream mirrors that contract on the read side.
type codec interface {
	EncodeStream(w io.Writer) (io.WriteCloser, error)
	DecodeStream(r io.Reader) (io.ReadCloser, error)
}

// newCodec returns the singleton codec for the given algorithm. Codecs
// are stateless wrappers around their library's encoder/decoder pools.
func newCodec(a Algo) (codec, error) {
	switch a {
	case AlgoZstd:
		return zstdCodec, nil
	case AlgoLZ4:
		return lz4Codec, nil
	default:
		return nil, fmt.Errorf("%w: %v", ErrUnsupportedCompressionAlgo, a)
	}
}

// --- zstd ---------------------------------------------------------------

var zstdCodec codec = &zstdImpl{}

type zstdImpl struct{}

var zstdEncoderPool = sync.Pool{
	New: func() any {
		// Discard the bound writer here; callers Reset() before use.
		enc, err := zstd.NewWriter(io.Discard,
			zstd.WithEncoderLevel(zstd.SpeedDefault),
			zstd.WithEncoderConcurrency(1),
		)
		if err != nil {
			// Library default level cannot fail to construct; surface
			// the error indirectly via a nil pool entry.
			return nil
		}
		return enc
	},
}

var zstdDecoderPool = sync.Pool{
	New: func() any {
		dec, err := zstd.NewReader(nil,
			zstd.WithDecoderConcurrency(1),
		)
		if err != nil {
			return nil
		}
		return dec
	},
}

func (zstdImpl) EncodeStream(w io.Writer) (io.WriteCloser, error) {
	v := zstdEncoderPool.Get()
	if v == nil {
		return nil, fmt.Errorf("compression: zstd encoder unavailable")
	}
	enc := v.(*zstd.Encoder)
	enc.Reset(w)
	return &zstdEncoderHandle{enc: enc}, nil
}

type zstdEncoderHandle struct {
	enc    *zstd.Encoder
	closed bool
}

func (h *zstdEncoderHandle) Write(p []byte) (int, error) {
	return h.enc.Write(p)
}

func (h *zstdEncoderHandle) Close() error {
	if h.closed {
		return nil
	}
	h.closed = true
	err := h.enc.Close()
	zstdEncoderPool.Put(h.enc)
	h.enc = nil
	return err
}

func (zstdImpl) DecodeStream(r io.Reader) (io.ReadCloser, error) {
	v := zstdDecoderPool.Get()
	if v == nil {
		return nil, fmt.Errorf("compression: zstd decoder unavailable")
	}
	dec := v.(*zstd.Decoder)
	if err := dec.Reset(r); err != nil {
		zstdDecoderPool.Put(dec)
		return nil, err
	}
	return &zstdDecoderHandle{dec: dec}, nil
}

type zstdDecoderHandle struct {
	dec    *zstd.Decoder
	closed bool
}

func (h *zstdDecoderHandle) Read(p []byte) (int, error) {
	return h.dec.Read(p)
}

func (h *zstdDecoderHandle) Close() error {
	if h.closed {
		return nil
	}
	h.closed = true
	// Reset to a nil reader so the decoder's internal state is released.
	_ = h.dec.Reset(nil)
	zstdDecoderPool.Put(h.dec)
	h.dec = nil
	return nil
}

// --- lz4 ----------------------------------------------------------------

var lz4Codec codec = &lz4Impl{}

type lz4Impl struct{}

var lz4WriterPool = sync.Pool{
	New: func() any {
		return lz4.NewWriter(io.Discard)
	},
}

var lz4ReaderPool = sync.Pool{
	New: func() any {
		return lz4.NewReader(nil)
	},
}

func (lz4Impl) EncodeStream(w io.Writer) (io.WriteCloser, error) {
	zw := lz4WriterPool.Get().(*lz4.Writer)
	zw.Reset(w)
	return &lz4EncoderHandle{w: zw}, nil
}

type lz4EncoderHandle struct {
	w      *lz4.Writer
	closed bool
}

func (h *lz4EncoderHandle) Write(p []byte) (int, error) {
	return h.w.Write(p)
}

func (h *lz4EncoderHandle) Close() error {
	if h.closed {
		return nil
	}
	h.closed = true
	err := h.w.Close()
	lz4WriterPool.Put(h.w)
	h.w = nil
	return err
}

func (lz4Impl) DecodeStream(r io.Reader) (io.ReadCloser, error) {
	zr := lz4ReaderPool.Get().(*lz4.Reader)
	zr.Reset(r)
	return &lz4DecoderHandle{r: zr}, nil
}

type lz4DecoderHandle struct {
	r      *lz4.Reader
	closed bool
}

func (h *lz4DecoderHandle) Read(p []byte) (int, error) {
	return h.r.Read(p)
}

func (h *lz4DecoderHandle) Close() error {
	if h.closed {
		return nil
	}
	h.closed = true
	// Reset binds reader to nil so the underlying source is released.
	h.r.Reset(nil)
	lz4ReaderPool.Put(h.r)
	h.r = nil
	return nil
}
