package metadata

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
	metaerrors "github.com/marmos91/dittofs/pkg/metadata/errors"
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
	// MS-FSCC §2.4.16 ("FileFullEaInformation")). Keys are EA names stored in the casing supplied by the
	// client; EA names are case-insensitive per NTFS semantics, so callers MUST resolve
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
	Blocks []block.ChunkRef `json:"blocks,omitempty"`

	// ManifestDirtyOffsets narrows a SetManifest write to the offsets that can
	// possibly differ from what the SQL backends already store, so their
	// manifest diff costs the changed range rather than the whole file.
	// Transient, request-scoped and never persisted; UpdateAttrs ignores it.
	//
	// nil means "unknown": the backend must diff the entire stored manifest.
	// Non-nil (empty included) is a promise that every offset outside the set
	// is already stored exactly as Blocks holds it — so the backend may read,
	// upsert and delete only within the set. Only a caller that folded a known
	// row set into an already-coherent Blocks projection can make that promise;
	// a caller that re-derived Blocks from scratch must leave this nil.
	ManifestDirtyOffsets []uint64 `json:"-"`

	// NewInode marks a File the caller has just constructed and knows has no
	// row yet, so a store that would otherwise probe for one can insert
	// straight away. Transient, request-scoped and never persisted. Only a
	// caller holding the create's exclusivity guarantee may set it: a store is
	// entitled to skip its existence check, and an inode that does exist then
	// surfaces as a duplicate-key error rather than an update.
	NewInode bool `json:"-"`

	// ExactAttrs marks a create whose attributes are the authoritative ones for
	// an entry that already existed, rather than a request for a new one. A
	// remove-then-recreate conversion passes it so the replacement keeps the
	// identity of the object it replaces: the create path's defaults (a
	// zero-mode default, the caller's UID/GID, SGID-parent inheritance) all
	// describe a *new* entry and would silently re-home or widen a
	// re-created one. Transient, request-scoped and never persisted.
	//
	// decision: setting this exempts the create from those defaults, from the
	// SGID-parent inheritance and from the non-root setid strip. The exemption
	// is safe because the flag is only ever set from attributes read off an
	// inode the caller just removed (carriedAttr), never from a create request
	// — the values are an existing entry's, already validated when it was
	// first created. Overturn if a caller can set it from wire input: it would
	// then let a client pin an arbitrary mode or owner past the type default.
	ExactAttrs bool `json:"-"`

	// ObjectID is the BLAKE3 Merkle root over ChunkRef.Hash values sorted
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
	ObjectID block.ObjectID `json:"object_id,omitempty"`

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

// CopyFileAttr creates a deep copy of a FileAttr structure.
//
// Useful when returning file attributes to callers to prevent
// external modification of internal state.
//
// The struct is copied by value so a field added to FileAttr is carried over
// without touching this function; only the reference-typed members below need
// explicit clones. Enumerating fields by hand here silently drops whatever the
// list has not kept up with.
func CopyFileAttr(attr *FileAttr) *FileAttr {
	if attr == nil {
		return nil
	}

	c := *attr

	if attr.ACL != nil {
		aclCopy := *attr.ACL
		aclCopy.ACEs = slices.Clone(attr.ACL.ACEs)
		aclCopy.SACL = slices.Clone(attr.ACL.SACL)
		c.ACL = &aclCopy
	}

	if attr.EAs != nil {
		eas := make(map[string][]byte, len(attr.EAs))
		for k, v := range attr.EAs {
			eas[k] = slices.Clone(v)
		}
		c.EAs = eas
	}

	c.Blocks = slices.Clone(attr.Blocks)
	c.ManifestDirtyOffsets = slices.Clone(attr.ManifestDirtyOffsets)

	if attr.DeletedAt != nil {
		deletedAt := *attr.DeletedAt
		c.DeletedAt = &deletedAt
	}

	return &c
}

// SetAttrs specifies which attributes to update in a SetFileAttributes call.
type SetAttrs struct {
	Mode *uint32
	// ModeOrMask, when non-nil, ORs the given bits into the stored Mode
	// atomically within SetFileAttributes. Used instead of Mode when only
	// specific bits should be set without reading the mode in the caller.
	ModeOrMask *uint32
	// ModeAndNotMask, when non-nil, clears the given bits from the stored
	// Mode atomically within SetFileAttributes.
	ModeAndNotMask *uint32
	UID            *uint32
	GID            *uint32
	Size           *uint64
	Atime          *time.Time
	Mtime          *time.Time
	AtimeNow       bool
	MtimeNow       bool
	CreationTime   *time.Time
	Ctime          *time.Time
	Hidden         *bool

	// PreserveCtime suppresses the automatic Ctime = now that any other change
	// in this SetAttrs would otherwise trigger, leaving the stored value exactly
	// as it is. This is not the same as naming Ctime: naming a value writes that
	// value, which drags the timestamp backwards whenever someone else advanced
	// it in the meantime. Use it for a change that must land without counting as
	// a metadata change for this file — an access-time bump on a handle whose
	// ChangeTime is frozen by the SMB -1 sentinel. Ignored when Ctime is also
	// set, since that is an explicit write and wins.
	PreserveCtime bool

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
// EA names, case-insensitively per NTFS semantics (MS-FSCC §2.4.16 ("FileFullEaInformation")
// defines the structure only); a set with a new name records that name's casing.
type EAMutation struct {
	// Name is the EA name (canonical NT form, no domain prefix).
	Name string
	// Value is the EA value bytes for an upsert. Ignored when Delete is true.
	Value []byte
	// Delete removes the named EA instead of upserting it.
	Delete bool
}

// LookupEA returns the value of the named extended attribute and whether it is
// present, resolving the name case-insensitively per NTFS semantics.
func (a *FileAttr) LookupEA(name string) ([]byte, bool) {
	key, found := findEAKey(a.EAs, name)
	if !found {
		return nil, false
	}
	return a.EAs[key], true
}

// ApplyEAMutations applies the supplied set/delete mutations to the file's EA
// map, resolving names case-insensitively. An upsert preserves the casing of an
// existing same-name EA (NTFS keeps the original casing); a brand new EA records
// the supplied casing. A delete removes any case-insensitive match. Deleting the
// last EA leaves the map nil so the omitempty wire form is preserved.
//
// It returns ErrXattrTooLarge, and leaves the file's EA map exactly as it was,
// when the result would encode to more than XattrTotalMaxBytes. All-or-nothing
// is what the SMB EA channel requires of a multi-entry set: MS-FSA
// §2.1.5.15.6 ("FileFullEaInformation") step 2.5 fails the operation and undoes
// every change it had made. It is also the only safe answer for the cap itself,
// since a partial apply would persist a set the next read has to tolerate but no
// write could produce.
//
// Every EA write in the tree lands here, which is why the bound lives here
// rather than at each caller: SetXattr passes one mutation, the SMB EA channel
// passes a whole decoded chain, and both must be bounded by the same rule.
func (a *FileAttr) ApplyEAMutations(muts []EAMutation) error {
	// Built beside the live map rather than folded into it, so a refusal needs
	// no rollback. ponytail: one map copy per EA write; the write it guards
	// already re-encodes the whole set, so this is not where the cost is.
	next := make(map[string][]byte, len(a.EAs)+len(muts))
	for k, v := range a.EAs {
		next[k] = v
	}

	// A mutation resolves its name against the mutations before it, not only
	// against the stored set, so two sets naming the same EA in one chain land
	// on one key exactly as an in-place fold did.
	sets := false
	for _, m := range muts {
		existingKey, found := findEAKey(next, m.Name)
		if m.Delete {
			if found {
				delete(next, existingKey)
			}
			continue
		}
		sets = true
		key := m.Name
		if found {
			key = existingKey
		}
		// Store a defensive copy so the caller's buffer cannot mutate the
		// stored value later. A nil value is normalised to a non-nil empty
		// slice so a zero-length EA round-trips as "present".
		val := make([]byte, len(m.Value))
		copy(val, m.Value)
		next[key] = val
	}
	if len(next) == 0 {
		next = nil
	}

	// decision: only a chain that sets something is measured. A delete can only
	// shrink the set, and refusing one would strand a file whose set already
	// exceeds the cap — recorded by a build that had no cap, which still decodes
	// — with no way to trim it back. Withdraw the exemption only if a delete can
	// ever grow the encoded form.
	if sets {
		size, err := encodedEABytes(next)
		if err != nil {
			return err
		}
		if size > XattrTotalMaxBytes {
			return ErrXattrTooLarge
		}
	}

	a.EAs = next
	return nil
}

// encodedEABytes reports the encoded size of an EA set, measured the way the
// backends store it: as one JSON object per file, values base64-encoded by
// encoding/json's []byte rule. Measuring the encoding rather than the raw value
// bytes is what makes the bound cover names and framing too — at ~22 bytes of
// object overhead per entry, a set of many tiny EAs is almost entirely framing,
// and a value-bytes-only bound would not see it at all.
func encodedEABytes(eas map[string][]byte) (int, error) {
	if len(eas) == 0 {
		return 0, nil
	}
	encoded, err := json.Marshal(eas)
	if err != nil {
		return 0, &StoreError{
			Code:    metaerrors.ErrInvalidArgument,
			Message: fmt.Sprintf("encode extended attributes: %v", err),
		}
	}
	return len(encoded), nil
}

// findEAKey returns the EA key in eas matching name case-insensitively, and
// whether a match exists.
//
// ponytail: an exact hit is a map lookup, a miss is a full scan, so applying a
// chain of n new names costs O(n²) case-folded compares. XattrTotalMaxBytes is
// what makes that a bounded cost rather than an open one; add a folded-key index
// beside the map only if a profile of a real EA chain shows the scan.
func findEAKey(eas map[string][]byte, name string) (string, bool) {
	if eas == nil {
		return "", false
	}
	if _, ok := eas[name]; ok {
		return name, true
	}
	for k := range eas {
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
