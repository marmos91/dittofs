package nfs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/metadata"
)

// SymlinkRequest represents an NFS SYMLINK request (RFC 1813 Section 3.3.10).
// The SYMLINK procedure creates a symbolic link.
type SymlinkRequest struct {
	// DirHandle is the file handle of the parent directory
	DirHandle []byte

	// Name is the name of the symbolic link to create
	Name string

	// Target is the path the symbolic link points to
	Target string

	// Attr contains attributes for the new symbolic link
	Attr metadata.SetAttrs
}

// SymlinkResponse represents an NFS SYMLINK response (RFC 1813 Section 3.3.10).
type SymlinkResponse struct {
	// Status is the NFS status code
	Status uint32

	// FileHandle is the handle of the created symlink (only if Status == NFS3OK)
	FileHandle []byte

	// Attr contains post-operation attributes of the symlink
	Attr *FileAttr

	// DirAttrBefore contains pre-operation weak cache consistency data for the directory
	DirAttrBefore *WccAttr

	// DirAttrAfter contains post-operation attributes for the directory
	DirAttrAfter *FileAttr
}

// Symlink creates a symbolic link.
//
// This implements the NFS SYMLINK procedure as defined in RFC 1813 Section 3.3.10.
// Symbolic links are special files that contain a path to another file or directory.
//
// Authentication: This layer performs no authentication. Auth checks should be
// performed by the caller based on the authenticated RPC credentials.
//
// Error Handling:
//   - Returns NFS3ErrNoEnt if the parent directory doesn't exist
//   - Returns NFS3ErrNotDir if DirHandle is not a directory
//   - Returns NFS3ErrExist if a file with the same name already exists
//   - Returns NFS3ErrIO for metadata repository errors
func (h *DefaultNFSHandler) Symlink(repository metadata.Repository, req *SymlinkRequest) (*SymlinkResponse, error) {
	logger.Debug("SYMLINK: creating '%s' -> '%s' in directory %x", req.Name, req.Target, req.DirHandle)

	// Get directory attributes
	dirAttr, err := repository.GetFile(metadata.FileHandle(req.DirHandle))
	if err != nil {
		logger.Warn("Directory not found: %v", err)
		return &SymlinkResponse{Status: NFS3ErrNoEnt}, nil
	}

	// Verify it's a directory
	if dirAttr.Type != metadata.FileTypeDirectory {
		logger.Warn("Handle is not a directory")
		return &SymlinkResponse{Status: NFS3ErrNotDir}, nil
	}

	// Capture pre-operation directory attributes for WCC
	wccDirAttr := captureWccAttr(dirAttr)

	// Check if file already exists
	_, err = repository.GetChild(metadata.FileHandle(req.DirHandle), req.Name)
	if err == nil {
		logger.Warn("File already exists: %s", req.Name)
		return &SymlinkResponse{
			Status:        NFS3ErrExist,
			DirAttrBefore: wccDirAttr,
		}, nil
	}

	// Create symlink attributes
	now := time.Now()
	symlinkAttr := &metadata.FileAttr{
		Type:          metadata.FileTypeSymlink,
		Mode:          0777, // Symlinks typically have 777 permissions
		UID:           0,
		GID:           0,
		Size:          uint64(len(req.Target)), // Size is the length of the target path
		Atime:         now,
		Mtime:         now,
		Ctime:         now,
		ContentID:     "",
		SymlinkTarget: req.Target, // Store the target path
	}

	// Apply optional attributes
	applySetAttrs(symlinkAttr, &req.Attr)

	// Add symlink to directory
	symlinkHandle, err := repository.AddFileToDirectory(
		metadata.FileHandle(req.DirHandle),
		req.Name,
		symlinkAttr,
	)
	if err != nil {
		logger.Error("Failed to create symlink: %v", err)
		return &SymlinkResponse{Status: NFS3ErrIO}, nil
	}

	// Update directory timestamps
	dirAttr.Mtime = now
	dirAttr.Ctime = now
	if err := repository.UpdateFile(metadata.FileHandle(req.DirHandle), dirAttr); err != nil {
		logger.Warn("Failed to update directory: %v", err)
	}

	// Generate response
	symlinkID := extractFileID(symlinkHandle)
	nfsSymlinkAttr := MetadataToNFSAttr(symlinkAttr, symlinkID)

	dirID := extractFileID(metadata.FileHandle(req.DirHandle))
	nfsDirAttr := MetadataToNFSAttr(dirAttr, dirID)

	logger.Info("SYMLINK created: %s -> %s", req.Name, req.Target)

	return &SymlinkResponse{
		Status:        NFS3OK,
		FileHandle:    symlinkHandle,
		Attr:          nfsSymlinkAttr,
		DirAttrBefore: wccDirAttr,
		DirAttrAfter:  nfsDirAttr,
	}, nil
}

// DecodeSymlinkRequest decodes an XDR-encoded SYMLINK request.
//
// The request format (RFC 1813 Section 3.3.10):
//
//	struct SYMLINK3args {
//	    diropargs3   where;
//	    symlinkdata3 symlink;
//	};
func DecodeSymlinkRequest(data []byte) (*SymlinkRequest, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("data too short: %d bytes", len(data))
	}

	reader := bytes.NewReader(data)

	// Decode directory handle
	dirHandle, err := decodeOpaque(reader)
	if err != nil {
		return nil, fmt.Errorf("decode directory handle: %w", err)
	}

	// Decode symlink name
	name, err := decodeString(reader)
	if err != nil {
		return nil, fmt.Errorf("decode name: %w", err)
	}

	// Decode attributes using the shared helper
	attr, err := decodeSetAttrs(reader)
	if err != nil {
		return nil, fmt.Errorf("decode attributes: %w", err)
	}

	// Decode target path
	target, err := decodeString(reader)
	if err != nil {
		return nil, fmt.Errorf("decode target: %w", err)
	}

	return &SymlinkRequest{
		DirHandle: dirHandle,
		Name:      name,
		Target:    target,
		Attr:      *attr,
	}, nil
}

// Encode encodes a SYMLINK response to XDR format.
//
// The response format (RFC 1813 Section 3.3.10):
//
//	struct SYMLINK3resok {
//	    post_op_fh3   obj;
//	    post_op_attr  obj_attributes;
//	    wcc_data      dir_wcc;
//	};
//
//	struct SYMLINK3resfail {
//	    wcc_data      dir_wcc;
//	};
func (resp *SymlinkResponse) Encode() ([]byte, error) {
	buf := new(bytes.Buffer)

	// Encode status
	if err := binary.Write(buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("encode status: %w", err)
	}

	if resp.Status == NFS3OK {
		// Encode post_op_fh3 (optional file handle)
		if err := encodeOptionalOpaque(buf, resp.FileHandle); err != nil {
			return nil, fmt.Errorf("encode file handle: %w", err)
		}

		// Encode post_op_attr (optional attributes)
		if err := encodeOptionalFileAttr(buf, resp.Attr); err != nil {
			return nil, fmt.Errorf("encode file attributes: %w", err)
		}
	}

	// Encode wcc_data for directory (always present)
	if err := encodeWccData(buf, resp.DirAttrBefore, resp.DirAttrAfter); err != nil {
		return nil, fmt.Errorf("encode directory wcc data: %w", err)
	}

	return buf.Bytes(), nil
}
