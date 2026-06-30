package block

// ChunkLocator resolves a chunk content hash to the physical location of its
// bytes in the remote store. It is the indirection layer that lets a chunk live
// either as its own standalone CAS object (today) or inside a larger "pack"
// object (#1414 object packing, PR3b).
//
//   - PackID == "" means the chunk is a standalone object at the canonical CAS
//     key cas/XX/YY/<hash> (see FormatCASKey) and occupies the WHOLE object.
//     Offset/Length are not consulted in this case — the read path GETs the
//     entire object and verifies it. This is the value EVERY chunk resolves to
//     today (and what a synced hash with no recorded locator defaults to), so
//     existing data needs no migration.
//   - PackID != "" means the chunk lives inside the pack object packs/<PackID>
//     (see FormatPackKey), and its wire bytes occupy [Offset, Offset+Length).
//     The read path issues a ranged GET against the pack object. Only PR3b's
//     packer ever produces such a locator.
type ChunkLocator struct {
	// PackID identifies the enclosing pack object. Empty means standalone.
	PackID string
	// Offset is the byte offset of the chunk's wire bytes within the pack
	// object. Zero (and unused) for standalone chunks.
	Offset int64
	// Length is the chunk's wire byte length within the pack object. Zero
	// (and unused) for standalone chunks, whose length is the whole object.
	Length int64
}

// IsStandalone reports whether the chunk is stored as its own CAS object
// (PackID == "") rather than inside a pack.
func (l ChunkLocator) IsStandalone() bool { return l.PackID == "" }

// PackKeyPrefix is the object-key prefix under which pack objects live, mirroring
// CASKeyPrefix ("cas/") for standalone chunk objects.
const PackKeyPrefix = "packs/"

// FormatPackKey returns the object key for a pack identified by packID:
// "packs/<packID>". It is the pack-object analogue of FormatCASKey for
// standalone chunk objects.
func FormatPackKey(packID string) string {
	return PackKeyPrefix + packID
}
