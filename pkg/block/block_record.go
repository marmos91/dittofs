package block

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
