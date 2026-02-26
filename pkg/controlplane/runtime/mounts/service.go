package mounts

import (
	"sync"
	"time"
)

// MountInfo represents an active protocol mount from a client.
type MountInfo struct {
	ClientAddr  string
	Protocol    string // "nfs" or "smb"
	ShareName   string
	MountedAt   time.Time
	AdapterData any // Protocol-specific details (e.g., NFS mount path, SMB session ID)
}

// Tracker provides thread-safe mount tracking across all protocol adapters.
type Tracker struct {
	mu     sync.RWMutex
	mounts map[string]*MountInfo // keyed by protocol:client:share
}

func NewTracker() *Tracker {
	return &Tracker{
		mounts: make(map[string]*MountInfo),
	}
}

func mountKey(protocol, clientAddr, shareName string) string {
	return protocol + ":" + clientAddr + ":" + shareName
}

func (mt *Tracker) Record(clientAddr, protocol, shareName string, adapterData any) {
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

func (mt *Tracker) Remove(clientAddr, protocol, shareName string) bool {
	mt.mu.Lock()
	defer mt.mu.Unlock()

	key := mountKey(protocol, clientAddr, shareName)
	if _, exists := mt.mounts[key]; exists {
		delete(mt.mounts, key)
		return true
	}
	return false
}

// RemoveByClient removes all mounts for a client address.
func (mt *Tracker) RemoveByClient(clientAddr string) bool {
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

func (mt *Tracker) RemoveAllByProtocol(protocol string) int {
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

func (mt *Tracker) RemoveAll() int {
	mt.mu.Lock()
	defer mt.mu.Unlock()

	count := len(mt.mounts)
	mt.mounts = make(map[string]*MountInfo)
	return count
}

// List returns copies of all active mount records.
func (mt *Tracker) List() []*MountInfo {
	return mt.collectMounts(nil)
}

func (mt *Tracker) ListByProtocol(protocol string) []*MountInfo {
	return mt.collectMounts(func(m *MountInfo) bool {
		return m.Protocol == protocol
	})
}

func (mt *Tracker) collectMounts(filter func(*MountInfo) bool) []*MountInfo {
	mt.mu.RLock()
	defer mt.mu.RUnlock()

	result := make([]*MountInfo, 0, len(mt.mounts))
	for _, mount := range mt.mounts {
		if filter != nil && !filter(mount) {
			continue
		}
		result = append(result, &MountInfo{
			ClientAddr:  mount.ClientAddr,
			Protocol:    mount.Protocol,
			ShareName:   mount.ShareName,
			MountedAt:   mount.MountedAt,
			AdapterData: mount.AdapterData,
		})
	}
	return result
}

func (mt *Tracker) Count() int {
	mt.mu.RLock()
	defer mt.mu.RUnlock()
	return len(mt.mounts)
}
