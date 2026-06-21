package metadata

import (
	"github.com/marmos91/dittofs/internal/logger"
)

// ============================================================================
// Service-level xattr operations (auth-context aware)
// ============================================================================
//
// These are the protocol-facing entry points: NFSv4.2 GETXATTR/SETXATTR/
// LISTXATTRS/REMOVEXATTR handlers (PR2) call these, mirroring how SMB EA writes
// flow through SetFileAttributes. Each applies a permission check consistent
// with the read/write data path, then delegates to the shared resolver in
// xattr.go which unifies the inline EA backing with named-stream child
// entities (precedence: stream-entity-wins-else-inline).
//
// Stream-backed VALUES are read through s.xattrStreamReader, wired by the
// runtime layer (which has block-store access). When unset, stream NAMES are
// still enumerable via ListXattr; GetXattr on a stream-only name reports
// not-found.

// SetXattrStreamReader installs the block-store-backed reader used to surface
// stream-backed xattr values. Wired once at startup by the runtime layer.
func (s *Service) SetXattrStreamReader(r StreamContentReader) { s.xattrStreamReader = r }

// GetXattr returns the value of the named xattr and whether it is present,
// resolved across both backings (stream wins) after a READ permission check.
func (s *Service) GetXattr(ctx *AuthContext, handle FileHandle, name string) ([]byte, bool, error) {
	store, err := s.storeForHandle(handle)
	if err != nil {
		return nil, false, err
	}
	if err := s.checkReadPermission(ctx, handle); err != nil {
		logger.Debug("GetXattr: read permission denied", "name", name, "error", err)
		return nil, false, err
	}
	return ResolveGetXattr(ctx.Context, store, handle, name, s.xattrStreamReader)
}

// SetXattr writes the xattr value into the inline backing when it fits
// (<= XattrInlineMaxBytes); a larger value returns ErrXattrTooLarge. Requires
// WRITE permission and is denied on a read-only share (per-user ceiling).
func (s *Service) SetXattr(ctx *AuthContext, handle FileHandle, name string, value []byte) error {
	store, err := s.storeForHandle(handle)
	if err != nil {
		return err
	}
	if ctx.ShareReadOnly {
		return &StoreError{Code: ErrAccessDenied, Message: "read-only share: xattr write denied"}
	}
	if err := s.checkWritePermission(ctx, handle); err != nil {
		logger.Debug("SetXattr: write permission denied", "name", name, "error", err)
		return err
	}
	return ResolveSetXattr(ctx.Context, store, handle, name, value)
}

// RemoveXattr removes the named xattr from the inline backing. Requires WRITE
// permission and is denied on a read-only share. Returns ErrNotFound when the
// name is absent from the inline backing.
func (s *Service) RemoveXattr(ctx *AuthContext, handle FileHandle, name string) error {
	store, err := s.storeForHandle(handle)
	if err != nil {
		return err
	}
	if ctx.ShareReadOnly {
		return &StoreError{Code: ErrAccessDenied, Message: "read-only share: xattr remove denied"}
	}
	if err := s.checkWritePermission(ctx, handle); err != nil {
		logger.Debug("RemoveXattr: write permission denied", "name", name, "error", err)
		return err
	}
	return ResolveRemoveXattr(ctx.Context, store, handle, name)
}

// ListXattr returns all xattr names on the file, merged from both backings,
// after a READ permission check.
func (s *Service) ListXattr(ctx *AuthContext, handle FileHandle) ([]string, error) {
	store, err := s.storeForHandle(handle)
	if err != nil {
		return nil, err
	}
	if err := s.checkReadPermission(ctx, handle); err != nil {
		logger.Debug("ListXattr: read permission denied", "error", err)
		return nil, err
	}
	return ResolveListXattr(ctx.Context, store, handle)
}
