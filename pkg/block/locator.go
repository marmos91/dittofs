package block

// ChunkLocator resolves a chunk content hash to the physical location of its
// bytes in the remote store: the enclosing packed block object blocks/<BlockID>
// (see FormatBlockKey) and the chunk's wire-byte window inside it.
//
// A zero locator (BlockID == "") is the standalone form written by releases
// that predate the packed-block format: the synced marker located the chunk as
// its own cas/ object. Nothing converts them any more, and the read path
// refuses them fail-closed rather than resolving an empty block key. Metadata
// backends still round-trip the zero form byte-compatibly, so a store holding
// them boots and reports the refusal per chunk instead of decoding garbage.
type ChunkLocator struct {
	// BlockID identifies the enclosing block object. Empty is the pre-packed
	// standalone form (see the type comment); the read path refuses it.
	BlockID string
	// WireOffset is the chunk's wire-byte offset within the block object.
	// Zero (and unused) for standalone chunks.
	WireOffset int64
	// WireLength is the chunk's wire-byte length within the block object.
	// Zero (and unused) for standalone chunks, whose length is the whole object.
	WireLength int64
}

// IsStandalone reports whether the locator is the standalone form
// (BlockID == ""): a marker written before the packed-block format, which no
// build still reading it can resolve. The live read path refuses such locators.
func (l ChunkLocator) IsStandalone() bool { return l.BlockID == "" }

// BlockKeyPrefix is the object-key prefix under which packed block objects live.
const BlockKeyPrefix = "blocks/"

// FormatBlockKey returns the object key for a block identified by blockID:
// "blocks/<blockID>".
func FormatBlockKey(blockID string) string {
	return BlockKeyPrefix + blockID
}
