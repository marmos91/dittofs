package metadata

import (
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
)

// File represents a file's complete identity and attributes.
type File struct {
	// ID is a unique identifier for this file.
	ID uuid.UUID `json:"id"`

	// ShareName is the share this file belongs to (e.g., "/export").
	ShareName string `json:"share_name"`

	// Path is the full path within the share (e.g., "/documents/report.pdf").
	Path string `json:"path"`

	// FileAttr is embedded for convenient access to attributes.
	FileAttr
}

// FileAttr contains the complete metadata for a file or directory.
type FileAttr struct {
	// Type is the file type (regular, directory, symlink, etc.)
	Type FileType `json:"type"`

	// Mode contains permission bits (0o7777 max)
	Mode uint32 `json:"mode"`

	// UID is the owner user ID
	UID uint32 `json:"uid"`

	// GID is the owner group ID
	GID uint32 `json:"gid"`

	// Nlink is the number of hard links referencing this file.
	Nlink uint32 `json:"nlink"`

	// Size is the file size in bytes
	Size uint64 `json:"size"`

	// Atime is the last access time
	Atime time.Time `json:"atime"`

	// Mtime is the last modification time (content changes)
	Mtime time.Time `json:"mtime"`

	// Ctime is the last change time (metadata changes)
	Ctime time.Time `json:"ctime"`

	// CreationTime is the file creation time (birth time).
	CreationTime time.Time `json:"creation_time"`

	// PayloadID is the identifier for retrieving file content.
	// This is the legacy path-based content identifier (e.g., "{shareName}/{path}").
	PayloadID PayloadID `json:"content_id"`

	// LinkTarget is the target path for symbolic links
	LinkTarget string `json:"link_target,omitempty"`

	// Rdev contains device major and minor numbers for device files.
	Rdev uint64 `json:"rdev,omitempty"`

	// Hidden indicates if the file should be hidden from directory listings.
	Hidden bool `json:"hidden,omitempty"`

	// ACL is the NFSv4 Access Control List for this file.
	// nil means no ACL is set -- use classic Unix permission check.
	// Non-nil with empty ACEs means an explicit empty ACL (denies all access).
	ACL *acl.ACL `json:"acl,omitempty"`

	// EAs holds the file's extended attributes (SMB FILE_FULL_EA_INFORMATION,
	// MS-FSCC §2.4.15). Keys are EA names stored in the casing supplied by the
	// client; per MS-FSCC EA names are case-insensitive, so callers MUST resolve
	// names case-insensitively (helpers on this type do so). Values are raw
	// bytes (may be empty but non-nil to distinguish a zero-length EA from an
	// absent one). nil means no EAs are set.
	//
	// Storage:
	//   - Postgres: eas JSONB column on files.
	//   - Badger: rides the JSON-encoded FileAttr blob.
	//   - Memory: typed map held directly (deep-copied on Put/Get).
	EAs map[string][]byte `json:"eas,omitempty"`

	// IdempotencyToken for detecting duplicate creation requests.
	IdempotencyToken uint64 `json:"idempotency_token,omitempty"`

	// Blocks is the authoritative content-addressed chunk list for this
	// file, sorted by Offset, populated at every sync finalization
	// Empty for directories, symlinks, and
	// legacy files that predate empty list triggers the
	// dual-read shim.
	//
	// Storage:
	// Postgres: separate file_block_refs join table.
	// Badger: rides existing JSON-encoded FileAttr blob.
	// Memory: typed slice held directly.
	Blocks []blockstore.BlockRef `json:"blocks,omitempty"`

	// ObjectID is the BLAKE3 Merkle root over BlockRef.Hash values sorted
	// by Offset, populated lazily at the post-Flush coordinator hook
	// (). All-zero sentinel means
	// "never quiesced": legacy files, partially-flushed
	// files (some blocks Pending), and freshly-mutated files awaiting
	// next quiesce. migration backfills.
	//
	// Storage:
	//   - Postgres: object_id BYTEA column on files + partial unique
	// index WHERE object_id IS NOT NULL.
	//   - Badger: rides existing JSON FileAttr blob; secondary key
	//     obj/{hex} -> file_id maintained on Put/Delete.
	//   - Memory: typed field; map[ContentHash]uuid index in store.
	ObjectID blockstore.ObjectID `json:"object_id,omitempty"`

	// DeletedAt is set when this node was recycled (moved into #recycle).
	// nil means the node is live. Drives retention reaping and trash listing.
	DeletedAt *time.Time `json:"deleted_at,omitempty"`

	// OriginalPath is the share-relative path the node occupied before being
	// recycled, WITHOUT a leading slash (e.g. "documents/report.pdf"). Used as
	// the default restore destination. Empty for live nodes.
	OriginalPath string `json:"original_path,omitempty"`

	// DeletedBy is the principal (AuthContext Identity.Username, or its UID as
	// a string when no username is known) that recycled the node. Display only.
	DeletedBy string `json:"deleted_by,omitempty"`
}

// SetAttrs specifies which attributes to update in a SetFileAttributes call.
type SetAttrs struct {
	Mode         *uint32
	UID          *uint32
	GID          *uint32
	Size         *uint64
	Atime        *time.Time
	Mtime        *time.Time
	AtimeNow     bool
	MtimeNow     bool
	CreationTime *time.Time
	Ctime        *time.Time
	Hidden       *bool

	// ACL sets the NFSv4 ACL on the file.
	// When non-nil, the ACL is validated (canonical ordering, max ACEs) before applying.
	ACL *acl.ACL

	// EAMutations applies extended-attribute set/delete operations to the
	// file's EA map. Each mutation either upserts (Delete=false) or removes
	// (Delete=true) a single EA name, resolved case-insensitively. nil/empty
	// leaves the EA map untouched. Applied atomically with the rest of the
	// SetAttrs under the store transaction.
	EAMutations []EAMutation
}

// EAMutation is a single extended-attribute upsert or delete, applied via
// SetAttrs.EAMutations. Name is matched case-insensitively against existing
// EA names (MS-FSCC §2.4.15); a set with a new name records that name's casing.
type EAMutation struct {
	// Name is the EA name (canonical NT form, no domain prefix).
	Name string
	// Value is the EA value bytes for an upsert. Ignored when Delete is true.
	Value []byte
	// Delete removes the named EA instead of upserting it.
	Delete bool
}

// LookupEA returns the value of the named extended attribute and whether it is
// present, resolving the name case-insensitively per MS-FSCC §2.4.15.
func (a *FileAttr) LookupEA(name string) ([]byte, bool) {
	key, found := a.findEAKey(name)
	if !found {
		return nil, false
	}
	return a.EAs[key], true
}

// ApplyEAMutations applies the supplied set/delete mutations to the file's EA
// map in place, resolving names case-insensitively. An upsert preserves the
// casing of an existing same-name EA (NTFS keeps the original casing); a brand
// new EA records the supplied casing. A delete removes any case-insensitive
// match. Deleting the last EA leaves the map nil so the omitempty wire form is
// preserved.
func (a *FileAttr) ApplyEAMutations(muts []EAMutation) {
	for _, m := range muts {
		existingKey, found := a.findEAKey(m.Name)
		if m.Delete {
			if found {
				delete(a.EAs, existingKey)
			}
			continue
		}
		if a.EAs == nil {
			a.EAs = make(map[string][]byte)
		}
		key := m.Name
		if found {
			key = existingKey
		}
		// Store a defensive copy so the caller's buffer cannot mutate the
		// stored value later. A nil value is normalised to a non-nil empty
		// slice so a zero-length EA round-trips as "present".
		val := make([]byte, len(m.Value))
		copy(val, m.Value)
		a.EAs[key] = val
	}
	if len(a.EAs) == 0 {
		a.EAs = nil
	}
}

// findEAKey returns the stored EA key matching name case-insensitively, and
// whether a match exists.
func (a *FileAttr) findEAKey(name string) (string, bool) {
	if a.EAs == nil {
		return "", false
	}
	if _, ok := a.EAs[name]; ok {
		return name, true
	}
	for k := range a.EAs {
		if strings.EqualFold(k, name) {
			return k, true
		}
	}
	return "", false
}

// FileType represents the type of a filesystem object.
type FileType int

const (
	FileTypeRegular FileType = iota
	FileTypeDirectory
	FileTypeSymlink
	FileTypeBlockDevice
	FileTypeCharDevice
	FileTypeSocket
	FileTypeFIFO
)

// PayloadID is an identifier for retrieving file content from the content repository.
type PayloadID string
