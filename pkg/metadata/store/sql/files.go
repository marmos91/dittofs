package sql

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
	"github.com/marmos91/dittofs/pkg/metadata/store/internal/sqlcodec"
)

// ============================================================================
// File and directory reads
// ============================================================================
//
// These are pure reads, so the pool path and the transaction path want the
// identical body: the only thing that has to change between them is which
// executor runs the statement, and that is what Core.X already is. Before this
// they existed four times over — a store copy and a transaction copy in each of
// the two dialects — which is how the pool path came to carry a ctx check the
// transaction path did not, and the transaction path a debug log the pool path
// did not.

// EncodeFileHandle builds a share-scoped handle from a stringified row id.
func EncodeFileHandle(shareName string, idStr string) (metadata.FileHandle, error) {
	id, err := uuid.Parse(idStr)
	if err != nil {
		return nil, err
	}
	return metadata.EncodeShareHandle(shareName, id)
}

// invalidHandle is the error every decode failure below reports. The message
// names what the handle was being used as, so a bad directory handle does not
// read as a bad file handle.
func invalidHandle(what string) error {
	return &metadata.StoreError{
		Code:    metadata.ErrInvalidHandle,
		Message: "invalid " + what + " handle",
	}
}

// GetFile retrieves file metadata by handle, reporting ErrNotFound when the
// handle names no row.
//
// FileAttr.Blocks is folded into this read by the dialect's block-ref
// aggregate rather than fetched by a second query. The aggregate yields NULL
// for directories, symlinks and blockless files, so their decoded slice stays
// nil.
func (c *Core) GetFile(ctx context.Context, handle metadata.FileHandle) (*metadata.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	shareName, id, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return nil, invalidHandle("file")
	}

	row := c.X.QueryRow(ctx, c.D.Files().GetFile, id, shareName)
	file, err := sqlcodec.FileRowToFileWithNlinkAndBlocks(row, true)
	if err != nil {
		return nil, c.D.MapError(err, "GetFile", "")
	}
	return file, nil
}

// GetChild resolves a name in a directory to the child's handle, reporting
// ErrNotFound when the name does not exist.
func (c *Core) GetChild(ctx context.Context, dirHandle metadata.FileHandle, name string) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	shareName, parentID, err := metadata.DecodeFileHandle(dirHandle)
	if err != nil {
		return nil, invalidHandle("directory")
	}

	var childID string
	if err := c.X.QueryRow(ctx, c.D.Files().GetChild, parentID, name).Scan(&childID); err != nil {
		return nil, c.D.MapError(err, "GetChild", name)
	}

	return EncodeFileHandle(shareName, childID)
}

// GetParent returns the parent handle for a file or directory, reporting
// ErrNotFound for a root directory because it has no parent edge.
func (c *Core) GetParent(ctx context.Context, handle metadata.FileHandle) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	shareName, childID, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return nil, invalidHandle("file")
	}

	var parentIDStr string
	if err := c.X.QueryRow(ctx, c.D.Files().GetParent, childID).Scan(&parentIDStr); err != nil {
		return nil, c.D.MapError(err, "GetParent", "")
	}

	return EncodeFileHandle(shareName, parentIDStr)
}

// GetLinkCount returns a file's hard link count, or 0 when the file does not
// exist. nlink is the sole source of truth; it is never recomputed from the
// parent edges.
func (c *Core) GetLinkCount(ctx context.Context, handle metadata.FileHandle) (uint32, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	_, fileID, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return 0, invalidHandle("file")
	}

	var count uint32
	err = c.X.QueryRow(ctx, c.D.Files().GetLinkCount, fileID).Scan(&count)
	if c.D.IsNoRows(err) {
		return 0, nil
	}
	if err != nil {
		// A fabricated 0 would let callers treat live content as unreferenced.
		return 0, c.D.MapError(err, "GetLinkCount", "")
	}

	return count, nil
}

// GetFileByPayloadID retrieves a file by its content ID, reporting ErrNotFound
// when no row carries it. Block refs are folded in as for GetFile.
func (c *Core) GetFileByPayloadID(ctx context.Context, payloadID metadata.PayloadID) (*metadata.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if payloadID == "" {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: "content ID cannot be empty",
		}
	}

	row := c.X.QueryRow(ctx, c.D.Files().GetFileByPayloadID, string(payloadID))
	file, err := sqlcodec.FileRowToFileWithNlinkAndBlocks(row, true)
	if err != nil {
		return nil, c.D.MapError(err, "GetFileByPayloadID", string(payloadID))
	}
	return file, nil
}

// listChildrenDefaultLimit is the page size applied when the caller passes a
// non-positive limit.
const listChildrenDefaultLimit = 1000

// ListChildren returns a page of directory entries plus the cursor for the
// next page, empty when the listing is exhausted.
//
// Each entry carries the attributes the listing row already holds, which is
// what lets READDIRPLUS answer without a GetFile per child. FileAttr.Blocks is
// deliberately NOT among them: the relational model keeps block refs in a
// separate table, and joining it per row would make a listing quadratic. The
// Memory and Badger backends do populate Blocks here, because their
// serialisation already carries the slice. Callers that need the ChunkRef list
// must re-read through GetFile.
func (c *Core) ListChildren(ctx context.Context, dirHandle metadata.FileHandle, cursor string, limit int) ([]metadata.DirEntry, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}

	shareName, parentID, err := metadata.DecodeFileHandle(dirHandle)
	if err != nil {
		return nil, "", invalidHandle("directory")
	}

	if limit <= 0 {
		limit = listChildrenDefaultLimit
	}

	rows, err := c.X.Query(ctx, c.D.Files().ListChildren, parentID, cursor, limit+1)
	if err != nil {
		return nil, "", c.D.MapError(err, "ListChildren", "")
	}
	defer rows.Close()

	var entries []metadata.DirEntry
	for rows.Next() && len(entries) < limit {
		var name, childIDStr string
		var fileType int16
		var mode, uid, gid int32
		var size int64
		var atime, mtime, ctime, creationTime int64
		var hidden bool
		var aclJSON []byte
		var easJSON []byte
		var objectIDRaw []byte
		var deletedAt sql.NullInt64
		var originalPath string
		var deletedBy string
		var linkCount sql.NullInt32

		err := rows.Scan(&name, &childIDStr, &fileType, &mode, &uid, &gid, &size,
			&atime, &mtime, &ctime, &creationTime, &hidden, &aclJSON, &easJSON, &objectIDRaw,
			&deletedAt, &originalPath, &deletedBy, &linkCount)
		if err != nil {
			return nil, "", err
		}

		childHandle, err := EncodeFileHandle(shareName, childIDStr)
		if err != nil {
			return nil, "", err
		}

		// A row with no inode join (nlink NULL) still has a type, so fall back
		// to the type's link count rather than reporting an unlinked entry.
		nlink := uint32(1)
		switch {
		case linkCount.Valid:
			nlink = uint32(linkCount.Int32)
		case metadata.FileType(fileType) == metadata.FileTypeDirectory:
			nlink = 2
		}

		attr := &metadata.FileAttr{
			Type:         metadata.FileType(fileType),
			Mode:         uint32(mode),
			Nlink:        nlink,
			UID:          uint32(uid),
			GID:          uint32(gid),
			Size:         uint64(size),
			Atime:        sqlcodec.FiletimeToTime(atime),
			Mtime:        sqlcodec.FiletimeToTime(mtime),
			Ctime:        sqlcodec.FiletimeToTime(ctime),
			CreationTime: sqlcodec.FiletimeToTime(creationTime),
			Hidden:       hidden,
		}
		if len(objectIDRaw) > 0 {
			if len(objectIDRaw) != block.HashSize {
				return nil, "", fmt.Errorf(
					"ListChildren: object_id has invalid length %d (want %d)",
					len(objectIDRaw), block.HashSize,
				)
			}
			copy(attr.ObjectID[:], objectIDRaw)
		}

		// Recycle-bin state travels on the listing row so trash enumeration
		// reflects it without a re-read.
		if deletedAt.Valid {
			t := sqlcodec.FiletimeToTime(deletedAt.Int64)
			attr.DeletedAt = &t
		}
		attr.OriginalPath = originalPath
		attr.DeletedBy = deletedBy

		// A malformed ACL or EA blob is treated as absent rather than failing
		// the whole listing, matching the GetFile decode path.
		if len(aclJSON) > 0 {
			var fileACL acl.ACL
			if err := json.Unmarshal(aclJSON, &fileACL); err == nil {
				attr.ACL = &fileACL
			}
		}
		if len(easJSON) > 0 {
			var eas map[string][]byte
			if err := json.Unmarshal(easJSON, &eas); err == nil && len(eas) > 0 {
				attr.EAs = eas
			}
		}

		entries = append(entries, metadata.DirEntry{
			ID:     metadata.HandleToINode(childHandle),
			Name:   name,
			Handle: childHandle,
			Attr:   attr,
		})
	}

	// Surface an error that terminated the iteration early. Without this a
	// partial result would be returned as a complete, successful listing.
	if err := rows.Err(); err != nil {
		return nil, "", c.D.MapError(err, "ListChildren", "")
	}

	nextCursor := ""
	if len(entries) >= limit {
		nextCursor = entries[len(entries)-1].Name
	}

	return entries, nextCursor, nil
}
