package encryption

import (
	"context"
	"fmt"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block"
)

// The encryption decorator's hash-keyed CAS forwards: ReadBlockVerified and the
// operations it rides. None of this is on the production RemoteStore surface,
// which is block-keyed.

// ReadBlockVerified GETs the standalone object, decrypts it, then re-verifies
// the BLAKE3 hash over the plaintext.
func (d *EncryptedRemote) ReadBlockVerified(ctx context.Context, hash block.ContentHash, expected block.ContentHash) ([]byte, error) {
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
