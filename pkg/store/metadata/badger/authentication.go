package badger

import (
	"context"
	"fmt"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// CheckShareAccess verifies if a client can access a share and returns effective credentials.
//
// This implements share-level access control including:
//   - IP-based access control (allowed/denied clients)
//   - Authentication method validation
//   - Identity mapping (squashing, anonymous access)
//
// The method uses a BadgerDB read transaction to retrieve the share configuration
// and perform all access control checks atomically.
//
// Thread Safety: Safe for concurrent use.
//
// Parameters:
//   - ctx: Context for cancellation
//   - shareName: Name of the share being accessed
//   - clientAddr: IP address of the client
//   - authMethod: Authentication method used (e.g., "unix", "anonymous")
//   - identity: Client's claimed identity (before mapping)
//
// Returns:
//   - *AccessDecision: Contains allowed status, reason, and share properties
//   - *AuthContext: Contains effective identity after mapping (use for subsequent operations)
//   - error: ErrNotFound if share doesn't exist, or context errors
func (s *BadgerMetadataStore) CheckShareAccess(
	ctx context.Context,
	shareName string,
	clientAddr string,
	authMethod string,
	identity *metadata.Identity,
) (*metadata.AccessDecision, *metadata.AuthContext, error) {
	// Check context cancellation
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	var decision *metadata.AccessDecision
	var authCtx *metadata.AuthContext

	err := s.db.View(func(txn *badger.Txn) error {
		// Step 1: Verify share exists
		item, err := txn.Get(keyShare(shareName))
		if err == badger.ErrKeyNotFound {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: fmt.Sprintf("share not found: %s", shareName),
				Path:    shareName,
			}
		}
		if err != nil {
			return fmt.Errorf("failed to get share: %w", err)
		}

		var shareData *shareData
		err = item.Value(func(val []byte) error {
			sd, err := decodeShareData(val)
			if err != nil {
				return err
			}
			shareData = sd
			return nil
		})
		if err != nil {
			return err
		}

		share := &shareData.Share
		opts := share.Options

		// Step 2: Check authentication requirements
		if opts.RequireAuth && authMethod == "anonymous" {
			decision = &metadata.AccessDecision{
				Allowed: false,
				Reason:  "authentication required but anonymous access attempted",
			}
			return nil // No error - this is a business decision
		}

		// Step 3: Validate authentication method
		if len(opts.AllowedAuthMethods) > 0 {
			methodAllowed := false
			for _, allowed := range opts.AllowedAuthMethods {
				if authMethod == allowed {
					methodAllowed = true
					break
				}
			}
			if !methodAllowed {
				decision = &metadata.AccessDecision{
					Allowed:            false,
					Reason:             fmt.Sprintf("authentication method '%s' not allowed", authMethod),
					AllowedAuthMethods: opts.AllowedAuthMethods,
				}
				return nil // No error - this is a business decision
			}
		}

		// Step 4: Check denied list first (deny takes precedence)
		if len(opts.DeniedClients) > 0 {
			for _, denied := range opts.DeniedClients {
				// Check context during iteration for large lists
				if len(opts.DeniedClients) > 10 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}

				if metadata.MatchesIPPattern(clientAddr, denied) {
					decision = &metadata.AccessDecision{
						Allowed: false,
						Reason:  fmt.Sprintf("client %s is explicitly denied", clientAddr),
					}
					return nil // No error - this is a business decision
				}
			}
		}

		// Step 5: Check allowed list (if specified)
		if len(opts.AllowedClients) > 0 {
			allowed := false
			for _, allowedPattern := range opts.AllowedClients {
				// Check context during iteration for large lists
				if len(opts.AllowedClients) > 10 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}

				if metadata.MatchesIPPattern(clientAddr, allowedPattern) {
					allowed = true
					break
				}
			}
			if !allowed {
				decision = &metadata.AccessDecision{
					Allowed: false,
					Reason:  fmt.Sprintf("client %s not in allowed list", clientAddr),
				}
				return nil // No error - this is a business decision
			}
		}

		// Step 6: Apply identity mapping
		effectiveIdentity := identity
		if identity != nil && opts.IdentityMapping != nil {
			effectiveIdentity = metadata.ApplyIdentityMapping(identity, opts.IdentityMapping)
		}

		// Step 7: Build successful access decision
		decision = &metadata.AccessDecision{
			Allowed:            true,
			Reason:             "",
			AllowedAuthMethods: opts.AllowedAuthMethods,
			ReadOnly:           opts.ReadOnly,
		}

		authCtx = &metadata.AuthContext{
			Context:    ctx,
			AuthMethod: authMethod,
			Identity:   effectiveIdentity,
			ClientAddr: clientAddr,
		}

		return nil
	})

	if err != nil {
		return nil, nil, err
	}

	return decision, authCtx, nil
}

// CheckPermissions performs file-level permission checking.
//
// This implements Unix-style permission checking based on file ownership,
// mode bits, and client credentials. The method uses a BadgerDB read transaction
// to retrieve file attributes and share configuration atomically.
//
// Thread Safety: Safe for concurrent use.
//
// Parameters:
//   - ctx: Authentication context with client credentials
//   - handle: The file handle to check permissions for
//   - requested: Bitmap of requested permissions
//
// Returns:
//   - Permission: Bitmap of granted permissions (subset of requested)
//   - error: Only for internal failures (file not found) or context cancellation
func (s *BadgerMetadataStore) CheckPermissions(
	ctx *metadata.AuthContext,
	handle metadata.FileHandle,
	requested metadata.Permission,
) (metadata.Permission, error) {
	// Check context before acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return 0, err
	}

	var granted metadata.Permission

	err := s.db.View(func(txn *badger.Txn) error {
		// Get file data
		_, id, err := metadata.DecodeFileHandle(handle)
		if err != nil {
			return &metadata.StoreError{
				Code:    metadata.ErrInvalidHandle,
				Message: "invalid file handle",
			}
		}
		item, err := txn.Get(keyFile(id))
		if err == badger.ErrKeyNotFound {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: "file not found",
			}
		}
		if err != nil {
			return fmt.Errorf("failed to get file: %w", err)
		}

		var file *metadata.File
		err = item.Value(func(val []byte) error {
			fd, err := decodeFile(val)
			if err != nil {
				return err
			}
			file = fd
			return nil
		})
		if err != nil {
			return err
		}

		identity := ctx.Identity

		// Handle anonymous/no identity case
		if identity == nil || identity.UID == nil {
			// Only grant "other" permissions for anonymous users
			granted = metadata.CheckOtherPermissions(file.Mode, requested)
			return nil
		}

		uid := *identity.UID
		gid := identity.GID

		// Root bypass: UID 0 gets all permissions EXCEPT on read-only shares
		if uid == 0 {
			// Check if share is read-only
			shareItem, err := txn.Get(keyShare(file.ShareName))
			if err == nil {
				err = shareItem.Value(func(val []byte) error {
					sd, err := decodeShareData(val)
					if err != nil {
						return err
					}
					if sd.Share.Options.ReadOnly {
						// Root gets all permissions except write on read-only shares
						granted = requested &^ (metadata.PermissionWrite | metadata.PermissionDelete)
					} else {
						// Root gets all permissions on normal shares
						granted = requested
					}
					return nil
				})
				if err != nil {
					return err
				}
			} else {
				// Share not found, grant all permissions (shouldn't happen)
				granted = requested
			}
			return nil
		}

		// Determine which permission bits apply
		var permBits uint32

		if uid == file.UID {
			// Owner permissions (bits 6-8)
			permBits = (file.Mode >> 6) & 0x7
		} else if gid != nil && (*gid == file.GID || identity.HasGID(file.GID)) {
			// Group permissions (bits 3-5)
			permBits = (file.Mode >> 3) & 0x7
		} else {
			// Other permissions (bits 0-2)
			permBits = file.Mode & 0x7
		}

		// Map Unix permission bits to Permission flags
		granted = metadata.CalculatePermissionsFromBits(permBits)

		// Owner gets additional privileges
		if uid == file.UID {
			granted |= metadata.PermissionChangePermissions | metadata.PermissionChangeOwnership
		}

		// Apply read-only share restriction for all non-root users
		shareItem, err := txn.Get(keyShare(file.ShareName))
		if err == nil {
			err = shareItem.Value(func(val []byte) error {
				sd, err := decodeShareData(val)
				if err != nil {
					return err
				}
				if sd.Share.Options.ReadOnly {
					granted &= ^(metadata.PermissionWrite | metadata.PermissionDelete)
				}
				return nil
			})
			if err != nil {
				return err
			}
		}

		return nil
	})

	if err != nil {
		return 0, err
	}

	return granted & requested, nil
}
