package block

// ChunkLocator resolves a chunk content hash to the physical location of its
// bytes in the remote store. It is the indirection layer that lets a chunk live
// either as its own standalone CAS object (today) or inside a larger "block"
// object (#1414 object packing, PR3b).
//
//   - BlockID == "" means the chunk is a standalone object at the canonical CAS
//     key cas/XX/YY/<hash> (see FormatCASKey) and occupies the WHOLE object.
//     WireOffset/WireLength are not consulted in this case — the read path GETs
//     the entire object and verifies it. This is the value EVERY chunk resolves
//     to today (and what a synced hash with no recorded locator defaults to), so
//     existing data needs no migration.
//   - BlockID != "" means the chunk lives inside the block object blocks/<BlockID>
//     (see FormatBlockKey), and its wire bytes occupy
//     [WireOffset, WireOffset+WireLength). The read path issues a ranged GET
//     against the block object. Only PR3b's packer ever produces such a locator.
type ChunkLocator struct {
	// BlockID identifies the enclosing block object. Empty means standalone.
	BlockID string
	// WireOffset is the chunk's wire-byte offset within the block object.
	// Zero (and unused) for standalone chunks.
	WireOffset int64
	// WireLength is the chunk's wire-byte length within the block object.
	// Zero (and unused) for standalone chunks, whose length is the whole object.
	WireLength int64
}

// IsStandalone reports whether the chunk is stored as its own CAS object
// (BlockID == "") rather than inside a block.
func (l ChunkLocator) IsStandalone() bool { return l.BlockID == "" }

// BlockKeyPrefix is the object-key prefix under which block objects live, mirroring
// CASKeyPrefix ("cas/") for standalone chunk objects.
const BlockKeyPrefix = "blocks/"

// FormatBlockKey returns the object key for a block identified by blockID:
// "blocks/<blockID>". It is the block-object analogue of FormatCASKey for
// standalone chunk objects.
func FormatBlockKey(blockID string) string {
	return BlockKeyPrefix + blockID
}
