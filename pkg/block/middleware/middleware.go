// Package middleware composes the transforms a block body passes through on
// its way to and from a remote store.
//
// The interface is generic: a Transform is any reversible per-chunk byte
// transform. The two that exist today are compression and encryption, in the
// subpackages of the same name, but nothing here knows that — a third stage is
// one Transform implementation and one more argument to [New].
package middleware

import (
	"context"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/remote"
)

// Transform is one stage of a block body's round trip. Seal runs on the way
// out, Open inverts it on the way back in.
//
// Both take ctx and hash because some stages need them: encryption binds hash
// as AEAD additional authenticated data. A stage that needs neither ignores
// both — compression does.
//
// A stage MUST frame its own output so that Open can recognise its own work,
// and MUST decide for itself what an unrecognised body means. That decision is
// per-stage and is NOT the pipeline's to make: compression treats an unframed
// body as plaintext and passes it through, because it skips its frame whenever
// the body would not shrink; encryption REJECTS an unframed body, because on an
// encryption-enabled share one means external mutation or a stale policy.
// Hoisting either rule into the pipeline would turn the second into the first
// and make encryption fail open.
//
// A Transform holding resources may implement [io.Closer]; [Pipeline.Close]
// closes every stage that does.
type Transform interface {
	Seal(ctx context.Context, hash block.ContentHash, data []byte) ([]byte, error)
	Open(ctx context.Context, hash block.ContentHash, data []byte) ([]byte, error)
}

// Pipeline is a remote.RemoteStore that runs a fixed stack of transforms around
// an inner store. Everything above it — engine, cache, GC, metadata — sees only
// plaintext, so a pipelined store is indistinguishable from a bare one.
//
// Only the two per-chunk operations are intercepted.
//
// decision: the block-keyed operations (PutBlock, GetBlock, GetBlockRange,
// DeleteBlock, WalkBlocks) forward untransformed via the embedded Passthrough,
// so a body written through them lands on the inner store exactly as given —
// plaintext, even on an encryption-enabled share. The rule holds because a
// packed block object carries per-chunk bodies that SealChunk has already
// transformed, and sealing the assembled block again would double-seal it;
// every caller today moves already-sealed bodies (the flush path packing chunks
// into a block, and compaction rewriting one). Withdraw the exemption the
// moment a caller hands PutBlock a body that did not come out of SealChunk.
type Pipeline struct {
	remote.Passthrough
	// inner is the wrapped store, held separately because Passthrough keeps
	// its own copy unexported to stay off this type's public surface.
	inner  remote.RemoteStore
	stages []Transform
}

// New builds a pipeline over inner. Stages are given in SEAL order, and Open
// runs them in reverse — so New(inner, compress, encrypt) writes
// compress-then-encrypt and reads decrypt-then-decompress.
//
// The pipeline takes ownership of its stages: Close closes every stage that
// implements io.Closer, so a stage must not be handed to two pipelines — the
// second Close would release a key provider another pipeline is still using.
//
// decision: stage order is not checked, and it is not checkable here — a
// Transform is an opaque byte transform, so nothing distinguishes compression
// from encryption at this layer. Either arrangement builds a working store that
// round-trips: the only symptom of a swap is a compression ratio stuck at ~1.0
// forever, because AEAD output has near-maximum entropy and does not compress.
// The rule holds only while the set of stages stays small enough for the
// construction sites to be audited by hand; give the order its own type once a
// stage arrives whose misordering corrupts data rather than wasting CPU, or
// once the construction sites outgrow shares.remoteStages.
func New(inner remote.RemoteStore, stages ...Transform) (*Pipeline, error) {
	if inner == nil {
		return nil, fmt.Errorf("middleware: inner RemoteStore is nil")
	}
	for i, s := range stages {
		if s == nil {
			return nil, fmt.Errorf("middleware: stage %d is nil", i)
		}
	}
	return &Pipeline{
		Passthrough: remote.NewPassthrough(inner),
		inner:       inner,
		stages:      append([]Transform(nil), stages...),
	}, nil
}

// SealChunk runs every stage in order over plaintext, then hands the fully
// transformed bytes to the inner store.
func (p *Pipeline) SealChunk(ctx context.Context, hash block.ContentHash, plaintext []byte) ([]byte, error) {
	data := plaintext
	for _, s := range p.stages {
		out, err := s.Seal(ctx, hash, data)
		if err != nil {
			return nil, err
		}
		data = out
	}
	return p.inner.SealChunk(ctx, hash, data)
}

// ReadChunk reads the chunk's wire bytes and inverts the stack in reverse.
//
// A block stores each chunk's fully sealed body verbatim, so opening a chunk's
// [offset, length) slice is identical to opening a standalone object. No
// verification happens here: no single stage holds both the wire bytes and the
// plaintext hash domain, so the engine hashes the recovered plaintext after the
// whole stack has run.
func (p *Pipeline) ReadChunk(ctx context.Context, blockID string, offset, length int64, hash block.ContentHash) ([]byte, error) {
	data, err := p.inner.ReadChunk(ctx, blockID, offset, length, hash)
	if err != nil {
		return nil, err
	}
	for i := len(p.stages) - 1; i >= 0; i-- {
		out, err := p.stages[i].Open(ctx, hash, data)
		if err != nil {
			return nil, err
		}
		data = out
	}
	return data, nil
}

// Close releases the inner store and every stage that holds resources. The
// inner store's error wins, because a stage that failed to close leaves the
// process holding a key handle while an inner failure can mean unflushed bytes.
// Every stage is closed even after one fails.
func (p *Pipeline) Close() error {
	innerErr := p.Passthrough.Close()
	var stageErr error
	for _, s := range p.stages {
		c, ok := s.(io.Closer)
		if !ok {
			continue
		}
		if err := c.Close(); err != nil && stageErr == nil {
			stageErr = err
		}
	}
	if innerErr != nil {
		return innerErr
	}
	return stageErr
}

// Compile-time interface assertions.
var (
	_ remote.RemoteStore       = (*Pipeline)(nil)
	_ remote.RemoteBlockStore  = (*Pipeline)(nil)
	_ remote.ChunkReader       = (*Pipeline)(nil)
	_ remote.ChunkSealer       = (*Pipeline)(nil)
	_ block.DurabilityReporter = (*Pipeline)(nil)
)
