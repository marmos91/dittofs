package runtime

import (
	"sync"
	"time"
)

// MountInfo represents an active protocol mount from a client.
// This is used by all protocol adapters (NFS, SMB) for unified mount tracking.
type MountInfo struct {
	ClientAddr  string    // Client IP address
	Protocol    string    // Protocol name: "nfs" or "smb"
	ShareName   string    // Name of the mounted share
	MountedAt   time.Time // When the mount was recorded
	AdapterData any       // Protocol-specific details (NFS mount path, SMB session ID, etc.)
}

// MountTracker provides thread-safe unified mount tracking across all protocol adapters.
// Each adapter records its mounts here, and the runtime exposes an aggregate view.
type MountTracker struct {
	mu     sync.RWMutex
	mounts map[string]*MountInfo // key: protocol:client:share
}

// NewMountTracker creates a new MountTracker.
func NewMountTracker() *MountTracker {
	return &MountTracker{
		mounts: make(map[string]*MountInfo),
	}
}

// mountKey generates the map key for a mount entry.
func mountKey(protocol, clientAddr, shareName string) string {
	return protocol + ":" + clientAddr + ":" + shareName
}

// Record registers that a client has mounted a share via a specific protocol.
func (mt *MountTracker) Record(clientAddr, protocol, shareName string, adapterData any) {
	mt.mu.Lock()
	defer mt.mu.Unlock()

	key := mountKey(protocol, clientAddr, shareName)
	mt.mounts[key] = &MountInfo{
		ClientAddr:  clientAddr,
		Protocol:    protocol,
		ShareName:   shareName,
		MountedAt:   time.Now(),
		AdapterData: adapterData,
	}
}

// Remove removes a mount record for the given client/protocol/share combination.
// Returns true if the mount was found and removed.
func (mt *MountTracker) Remove(clientAddr, protocol, shareName string) bool {
	mt.mu.Lock()
	defer mt.mu.Unlock()

	key := mountKey(protocol, clientAddr, shareName)
	if _, exists := mt.mounts[key]; exists {
		delete(mt.mounts, key)
		return true
	}
	return false
}

// RemoveByClient removes a mount record by client address only (legacy NFS-style keying).
// This supports the NFS MOUNT protocol where mounts are keyed by client IP alone.
// Returns true if any mount was found and removed.
func (mt *MountTracker) RemoveByClient(clientAddr string) bool {
	mt.mu.Lock()
	defer mt.mu.Unlock()

	found := false
	for key, info := range mt.mounts {
		if info.ClientAddr == clientAddr {
			delete(mt.mounts, key)
			found = true
		}
	}
	return found
}

// RemoveAllByProtocol removes all mount records for a given protocol.
// Returns the number of mounts removed.
func (mt *MountTracker) RemoveAllByProtocol(protocol string) int {
	mt.mu.Lock()
	defer mt.mu.Unlock()

	count := 0
	for key, info := range mt.mounts {
		if info.Protocol == protocol {
			delete(mt.mounts, key)
			count++
		}
	}
	return count
}

// RemoveAll removes all mount records across all protocols.
// Returns the number of mounts removed.
func (mt *MountTracker) RemoveAll() int {
	mt.mu.Lock()
	defer mt.mu.Unlock()

	count := len(mt.mounts)
	mt.mounts = make(map[string]*MountInfo)
	return count
}

// List returns all active mount records across all protocols.
// The returned slice is a copy; callers may safely modify it.
func (mt *MountTracker) List() []*MountInfo {
	mt.mu.RLock()
	defer mt.mu.RUnlock()

	mounts := make([]*MountInfo, 0, len(mt.mounts))
	for _, mount := range mt.mounts {
		mounts = append(mounts, &MountInfo{
			ClientAddr:  mount.ClientAddr,
			Protocol:    mount.Protocol,
			ShareName:   mount.ShareName,
			MountedAt:   mount.MountedAt,
			AdapterData: mount.AdapterData,
		})
	}
	return mounts
}

// ListByProtocol returns mount records for a specific protocol.
func (mt *MountTracker) ListByProtocol(protocol string) []*MountInfo {
	mt.mu.RLock()
	defer mt.mu.RUnlock()

	mounts := make([]*MountInfo, 0)
	for _, mount := range mt.mounts {
		if mount.Protocol == protocol {
			mounts = append(mounts, &MountInfo{
				ClientAddr:  mount.ClientAddr,
				Protocol:    mount.Protocol,
				ShareName:   mount.ShareName,
				MountedAt:   mount.MountedAt,
				AdapterData: mount.AdapterData,
			})
		}
	}
	return mounts
}

// Count returns the total number of active mounts.
func (mt *MountTracker) Count() int {
	mt.mu.RLock()
	defer mt.mu.RUnlock()
	return len(mt.mounts)
}
