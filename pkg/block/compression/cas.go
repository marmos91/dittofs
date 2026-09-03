package compression

import (
	"context"
	"fmt"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block"
)

// Migration-only legacy standalone-CAS forwards (#1493 PR4): the compression
// decorator's remote.LegacyCASStore implementation plus the hash-keyed
// ReadBlockVerified it rides. ReadLegacyChunkVerified decodes the stored bytes
// (and, further down the stack, decrypts) exactly as the old standalone read
// path did. None of this is on the production RemoteStore surface. Delete when
// the cas→blocks migration is retired.

// ReadBlockVerified GETs the standalone object, decodes it, then re-verifies
// the BLAKE3 hash over the plaintext. Streaming verification over the wire
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
		return nil, fmt.Errorf("%w: got %s want %s", block.ErrChunkContentMismatch, got, expected)
	}
	return plain, nil
}
