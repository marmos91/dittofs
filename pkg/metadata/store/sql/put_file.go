package sql

import (
	"database/sql"
	"encoding/json"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/basestore"
	"github.com/marmos91/dittofs/pkg/metadata/store/internal/sqlcodec"
)

// InodeValues are the twenty column values an inode write sends, in the order
// both the UPDATE and the INSERT name them. Keeping one builder for both means
// a column added to the table cannot be wired into one statement and forgotten
// in the other.
//
// The dialects place them differently — the UPDATE appends the id and share
// name as the WHERE arguments, the INSERT prepends them as the first two
// columns — so the caller adds those, and only those.
func InodeValues(file *metadata.File) ([]any, error) {
	// Device numbers are only meaningful for device nodes; every other type
	// stores SQL NULL rather than a zero that would read back as major 0.
	var deviceMajor, deviceMinor *int32
	if file.Type == metadata.FileTypeBlockDevice || file.Type == metadata.FileTypeCharDevice {
		major := int32(metadata.RdevMajor(file.Rdev))
		minor := int32(metadata.RdevMinor(file.Rdev))
		deviceMajor = &major
		deviceMinor = &minor
	}

	var payloadIDPtr *string
	if file.PayloadID != "" {
		str := string(file.PayloadID)
		payloadIDPtr = &str
	}

	var linkTargetPtr *string
	if file.LinkTarget != "" {
		linkTargetPtr = &file.LinkTarget
	}

	// ACL and EAs are stored as JSON. Empty or nil writes SQL NULL, so a file
	// that never had either stores nothing rather than an empty document.
	var aclJSON []byte
	if file.ACL != nil {
		var err error
		if aclJSON, err = json.Marshal(file.ACL); err != nil {
			return nil, err
		}
	}

	var easJSON []byte
	if len(file.EAs) > 0 {
		var err error
		if easJSON, err = json.Marshal(file.EAs); err != nil {
			return nil, err
		}
	}

	// A zero-valued ObjectID writes SQL NULL so the partial unique index
	// (files_object_id_idx WHERE object_id IS NOT NULL) skips the row —
	// legacy, never-quiesced and partially-flushed files must not collide on
	// the all-zero sentinel.
	var objectIDArg any
	if !file.ObjectID.IsZero() {
		objectIDArg = file.ObjectID[:]
	}

	// deleted_at is a nullable Windows-FILETIME column: NULL marks a live
	// node, a value records the recycle instant losslessly. It has to travel
	// through the same encoding as the other timestamps or it will not decode
	// back through FiletimeToTime.
	var deletedAtArg *int64
	if file.DeletedAt != nil {
		n := sqlcodec.TimeToFiletime(*file.DeletedAt)
		deletedAtArg = &n
	}

	return []any{
		file.Type, file.Mode, file.UID, file.GID, file.Size,
		sqlcodec.TimeToFiletime(file.Atime), sqlcodec.TimeToFiletime(file.Mtime),
		sqlcodec.TimeToFiletime(file.Ctime), sqlcodec.TimeToFiletime(file.CreationTime),
		payloadIDPtr, linkTargetPtr, deviceMajor, deviceMinor,
		file.Hidden, aclJSON, easJSON, objectIDArg,
		deletedAtArg, file.OriginalPath, file.DeletedBy,
	}, nil
}

// OldInode is the pre-update row an inode write reads so it can compute the
// usage delta. Each dialect obtains it differently — postgres returns it from
// the UPDATE's CTE, sqlite selects it first — but what it means is the same.
type OldInode struct {
	Size, UID, GID, Type, Nlink sql.NullInt64
}

// ApplyPutQuota records the usage change an inode write produces.
//
// Only an update charges a delta; an insert is charged by the caller once the
// row exists. nlink is not among the columns a write touches, so the
// pre-update count is also the post-update one and it is safe to decide
// chargeability from the old row.
func ApplyPutQuota(quota *basestore.QuotaDelta, file *metadata.File, old OldInode) {
	if !basestore.Charged(file.Type, uint32(old.Nlink.Int64)) {
		return
	}

	var oldSize uint64
	if old.Size.Valid {
		oldSize = uint64(old.Size.Int64)
	}

	// The previous row need not have been a regular file: after a type change
	// it contributed nothing before, so the write adds the whole size.
	oldWasRegular := old.Type.Valid && metadata.FileType(old.Type.Int64) == metadata.FileTypeRegular
	oldUID := uint32(old.UID.Int64)
	oldGID := uint32(old.GID.Int64)

	switch {
	case !oldWasRegular:
		quota.Add(file.ShareName, file.UID, file.GID, int64(file.Size), 1)
	case oldUID == file.UID && oldGID == file.GID:
		quota.Add(file.ShareName, file.UID, file.GID, int64(file.Size)-int64(oldSize), 0)
	default:
		// Chown: the bytes and the inode move from the old owner to the new.
		quota.Add(file.ShareName, oldUID, oldGID, -int64(oldSize), -1)
		quota.Add(file.ShareName, file.UID, file.GID, int64(file.Size), 1)
	}
}
