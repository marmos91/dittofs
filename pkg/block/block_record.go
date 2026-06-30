package block

// BlockRecord tracks the lifecycle and sync state of a single log-blob block.
type BlockRecord struct {
	BlockID        string
	BlockHash      ContentHash
	Length         int64
	LiveChunkCount uint32
	SyncState      BlockState
}

// LocalChunkLocation identifies the position of a chunk within a local log-blob.
type LocalChunkLocation struct {
	LogBlobID string
	RawOffset int64
	RawLength int64
}

// BlockChunkCommit bundles everything needed to persist one chunk's locations
// when a block is committed.
type BlockChunkCommit struct {
	Hash   ContentHash
	Remote ChunkLocator
	Local  LocalChunkLocation
}
