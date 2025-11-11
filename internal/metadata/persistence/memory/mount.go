package memory

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/internal/metadata"
)

// RecordMount records an active mount by a client with authentication details.
//
// This tracks:
//   - Which export was mounted
//   - Which client mounted it
//   - What authentication was used
//   - When the mount occurred
//
// If the client already has an active mount of this export, the mount
// record is updated with the new information.
//
// Parameters:
//   - exportPath: Path of the mounted export
//   - clientAddr: IP address of the client
//   - authFlavor: Authentication flavor used (AUTH_NULL, AUTH_UNIX, etc.)
//   - machineName: Client machine name (from mount protocol)
//   - uid: Unix UID (may be nil for AUTH_NULL)
//   - gid: Unix GID (may be nil for AUTH_NULL)
//
// Returns:
//   - error: Always returns nil (reserved for future validation)
func (r *MemoryRepository) RecordMount(ctx context.Context, exportPath string, clientAddr string, authFlavor uint32, machineName string, uid *uint32, gid *uint32) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := mountKey{exportPath: exportPath, clientAddr: clientAddr}

	entry := &metadata.MountEntry{
		ExportPath:  exportPath,
		ClientAddr:  clientAddr,
		MountedAt:   time.Now(),
		AuthFlavor:  authFlavor,
		MachineName: machineName,
		UnixUID:     uid,
		UnixGID:     gid,
	}

	// Update if already exists
	r.mounts[key] = entry
	return nil
}

// RemoveMount removes a mount record when a client unmounts.
//
// This should be called when:
//   - Client explicitly unmounts (UMNT procedure)
//   - Connection is lost
//   - Export is unexported
//
// Removing a mount record does NOT disconnect the client - it only
// updates the tracking state.
//
// Parameters:
//   - exportPath: Path of the export
//   - clientAddr: IP address of the client
//
// Returns:
//   - error: Always returns nil (idempotent operation)
func (r *MemoryRepository) RemoveMount(ctx context.Context, exportPath string, clientAddr string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := mountKey{exportPath: exportPath, clientAddr: clientAddr}
	delete(r.mounts, key)
	return nil
}

// GetMounts returns all active mounts, optionally filtered by export path.
//
// This is used by:
//   - DUMP procedure to list all mounts
//   - Monitoring and diagnostics
//   - Export unexport operations
//
// Parameters:
//   - exportPath: Filter by export path (empty string = all mounts)
//
// Returns:
//   - []MountEntry: List of matching mount records
//   - error: Always returns nil
func (r *MemoryRepository) GetMounts(ctx context.Context, exportPath string) ([]metadata.MountEntry, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]metadata.MountEntry, 0)
	for key, mount := range r.mounts {
		if exportPath == "" || key.exportPath == exportPath {
			result = append(result, *mount)
		}
	}

	return result, nil
}

// IsClientMounted checks if a specific client has an active mount.
//
// This is used for:
//   - Duplicate mount detection
//   - Connection state tracking
//   - Access control decisions
//
// Parameters:
//   - exportPath: Path of the export
//   - clientAddr: IP address of the client
//
// Returns:
//   - bool: True if client has an active mount
//   - error: Always returns nil
func (r *MemoryRepository) IsClientMounted(ctx context.Context, exportPath string, clientAddr string) (bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	key := mountKey{exportPath: exportPath, clientAddr: clientAddr}
	_, exists := r.mounts[key]
	return exists, nil
}

// GetMountsByClient returns all active mounts for a specific client.
//
// This is used by UMNTALL to determine which mounts will be removed
// before actually removing them.
//
// Parameters:
//   - clientAddr: IP address of the client
//
// Returns:
//   - []MountEntry: List of all mounts by this client
//   - error: Always returns nil
func (r *MemoryRepository) GetMountsByClient(ctx context.Context, clientAddr string) ([]metadata.MountEntry, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]metadata.MountEntry, 0)
	for key, mount := range r.mounts {
		if key.clientAddr == clientAddr {
			result = append(result, *mount)
		}
	}

	return result, nil
}

// RemoveAllMounts removes all mount records for a specific client.
//
// This is used by the UMNTALL procedure to clean up all of a client's
// mounts in a single operation. This is more efficient than calling
// RemoveMount repeatedly.
//
// The operation is atomic - either all mounts are removed or none are
// (though in practice this always succeeds).
//
// Parameters:
//   - clientAddr: IP address of the client
//
// Returns:
//   - error: Always returns nil
func (r *MemoryRepository) RemoveAllMounts(ctx context.Context, clientAddr string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Find all mount keys for this client
	keysToDelete := make([]mountKey, 0)
	for key := range r.mounts {
		if key.clientAddr == clientAddr {
			keysToDelete = append(keysToDelete, key)
		}
	}

	// Delete all found mounts
	for _, key := range keysToDelete {
		delete(r.mounts, key)
	}

	return nil
}
