package block

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// BlockRecord tracks the lifecycle and sync state of a single log-blob block.
type BlockRecord struct {
	BlockID        string
	BlockHash      ContentHash
	Length         int64
	LiveChunkCount uint32
	SyncState      BlockState
}

// BlockChunkCommit bundles everything needed to persist one chunk's locators
// when a block is committed.
type BlockChunkCommit struct {
	Hash   ContentHash
	Remote ChunkLocator
}

// NewBlockID returns a fresh, unguessable block object key. crypto/rand keeps it
// collision-free under concurrent writers (unlike a timestamp) and unrelated to
// the block's content hash, so a re-carve after a crash always targets a new
// object.
func NewBlockID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate block id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
