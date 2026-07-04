package block

// ChunkLocator resolves a chunk content hash to the physical location of its
// bytes in the remote store: the enclosing packed block object blocks/<BlockID>
// (see FormatBlockKey) and the chunk's wire-byte window inside it.
//
// A zero locator (BlockID == "") is the LEGACY standalone form: pre-#1493
// synced markers located the chunk as its own cas/ object. The live read path
// refuses such locators (post-migration drift, fail-closed); only the one-shot
// cas→blocks startup migration consumes them, rewriting each to a block
// locator. Metadata backends still round-trip the zero form byte-compatibly
// so un-migrated stores can boot into the migration.
type ChunkLocator struct {
	// BlockID identifies the enclosing block object. Empty is the legacy
	// standalone form (see the type comment); the read path refuses it.
	BlockID string
	// WireOffset is the chunk's wire-byte offset within the block object.
	// Zero (and unused) for standalone chunks.
	WireOffset int64
	// WireLength is the chunk's wire-byte length within the block object.
	// Zero (and unused) for standalone chunks, whose length is the whole object.
	WireLength int64
}

// IsStandalone reports whether the locator is the legacy standalone form
// (BlockID == ""): a pre-#1493 marker not yet rewritten by the cas→blocks
// migration. The live read path refuses such locators.
func (l ChunkLocator) IsStandalone() bool { return l.BlockID == "" }

// BlockKeyPrefix is the object-key prefix under which packed block objects live.
const BlockKeyPrefix = "blocks/"

// FormatBlockKey returns the object key for a block identified by blockID:
// "blocks/<blockID>".
func FormatBlockKey(blockID string) string {
	return BlockKeyPrefix + blockID
}
