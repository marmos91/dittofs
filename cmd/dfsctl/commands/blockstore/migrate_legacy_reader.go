package blockstore

import (
	"context"
	"errors"
	"io"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// legacyPayloadReader is an io.Reader wrapping the file's legacy
// {payloadID}/block-{idx} keys in order. The migration loop hands this
// to the FastCDC chunker, which re-cuts the byte stream into CAS chunks.
//
// Reads are pull-based: each Read() drains any leftover from the
// previously-fetched legacy block first, then fetches the next one.
// Sparse blocks (ErrBlockNotFound) yield zero-filled bytes of the
// expected DataSize — same semantic the dual-read shim uses for legacy
// reads against a hole.
type legacyPayloadReader struct {
	ctx      context.Context
	rt       *offlineRuntime
	blocks   []*blockstore.FileBlock
	idx      int
	leftover []byte
}

// newLegacyPayloadReader enumerates the file's legacy FileBlock rows
// via metadata.MetadataStore.ListFileBlocks and returns a Reader over
// them in index order.
//
// A file with zero legacy blocks (post-CAS files; legacy zero-byte
// files) yields an immediately-EOF reader.
func newLegacyPayloadReader(ctx context.Context, rt *offlineRuntime, payloadID string) (*legacyPayloadReader, error) {
	blocks, err := rt.MetadataStore().ListFileBlocks(ctx, payloadID)
	if err != nil {
		return nil, err
	}
	return &legacyPayloadReader{
		ctx:    ctx,
		rt:     rt,
		blocks: blocks,
	}, nil
}

func (r *legacyPayloadReader) Read(p []byte) (int, error) {
	if len(r.leftover) > 0 {
		n := copy(p, r.leftover)
		r.leftover = r.leftover[n:]
		return n, nil
	}
	if r.idx >= len(r.blocks) {
		return 0, io.EOF
	}
	block := r.blocks[r.idx]
	r.idx++

	// Fetch via the legacy ReadBlock (zero-hash) path. The block's
	// legacy key is its FileBlock ID for v0.13/v0.14 data, which is
	// "{payloadID}/block-{idx}". For mixed/CAS blocks (post-Phase-12
	// data being re-migrated — atypical but possible) the BlockStoreKey
	// carries the canonical key.
	key := block.BlockStoreKey
	if key == "" {
		key = block.ID
	}
	data, rerr := r.rt.RemoteStore().ReadBlock(r.ctx, key)
	if rerr != nil {
		// Sparse / missing block → zero-fill of the expected size. The
		// chunker treats zeros as ordinary bytes and will produce the
		// canonical zero-payload BlockRefs that subsequent reads dedup
		// against.
		if errors.Is(rerr, blockstore.ErrBlockNotFound) && block.DataSize > 0 {
			data = make([]byte, block.DataSize)
		} else {
			return 0, rerr
		}
	}

	n := copy(p, data)
	if n < len(data) {
		r.leftover = data[n:]
	}
	return n, nil
}
