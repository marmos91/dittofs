package memory

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/backup"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

const (
	// memoryEngineTag identifies this backup stream as originating from
	// the memory metadata store.
	memoryEngineTag = "memory"

	// memorySchemaVersion is the schema version written into every
	// memory backup payload. Restore rejects streams with a different
	// version to prevent silent data corruption across code changes.
	memorySchemaVersion = uint32(1)

	// maxRestorePayloadSize is the maximum single allocation size
	// (256 MiB) permitted for the gob payload during restore. Prevents
	// OOM from crafted streams with bogus payloadLen fields.
	maxRestorePayloadSize = 256 << 20
)

// memoryBackupSnapshot is the gob-encoded payload written inside the
// envelope. All fields are exported so gob can encode/decode them.
// The struct mirrors the data portion of MemoryMetadataStore; transient
// caches (sessions, attrPool) and config limits
// (maxStorageBytes, maxFiles) are excluded.
type memoryBackupSnapshot struct {
	Shares        map[string]*shareData
	Files         map[string]*fileData
	Parents       map[string]metadata.FileHandle
	Children      map[string]map[string]metadata.FileHandle
	LinkCounts    map[string]uint32
	DeviceNumbers map[string]*deviceNumber
	PendingWrites map[string]*metadata.WriteOperation
	ServerConfig  metadata.MetadataServerConfig
	Capabilities  metadata.FilesystemCapabilities
	StoreID       string
	RollupOffsets map[string]uint64
	Synced        map[block.ContentHash]time.Time
	ObjectIndex   map[block.ContentHash]string

	// ServerConfigCustomSettingsJSON holds the JSON-encoded form of
	// ServerConfig.CustomSettings. Gob cannot encode map[string]any
	// out of the box; we JSON-encode here and decode on restore.
	ServerConfigCustomSettingsJSON []byte

	// Lazy sub-store snapshots (nil when sub-store was never initialized).
	FileBlockData  *fileBlockSnapshotData
	Locks          *lockSnapshotData
	Clients        *clientSnapshotData
	DurableHandles *durableSnapshotData
}

// fileBlockSnapshotData is a gob-friendly copy of fileBlockStoreData.
type fileBlockSnapshotData struct {
	Blocks    map[string]*metadata.FileBlock
	HashIndex map[metadata.ContentHash]string
}

// lockSnapshotData is a gob-friendly copy of memoryLockStore.
type lockSnapshotData struct {
	Locks       map[string]*lock.PersistedLock
	ServerEpoch uint64
}

// clientSnapshotData is a gob-friendly copy of memoryClientStore.
type clientSnapshotData struct {
	Registrations map[string]*lock.PersistedClientRegistration
}

// durableSnapshotData is a gob-friendly copy of memoryDurableStore.
type durableSnapshotData struct {
	Handles map[string]*lock.PersistedDurableHandle
}

// Compile-time assertion: MemoryMetadataStore implements Backupable.
var _ metadata.Backupable = (*MemoryMetadataStore)(nil)

// Backup serializes the entire in-memory state under a read lock and
// writes it into w using the shared envelope format. The returned
// HashSet contains every unique content-addressed block hash referenced
// by the snapshot (extracted inline during serialization).
func (s *MemoryMetadataStore) Backup(ctx context.Context, w io.Writer) (*block.HashSet, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", metadata.ErrBackupAborted, err)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	// Build the snapshot from live store fields. The read lock
	// guarantees a consistent view; gob encodes from the pointers
	// held in the snapshot, so we keep the lock through encoding
	// to ensure snapshot isolation (ConcurrentWriter conformance).
	snap := memoryBackupSnapshot{
		Shares:        s.shares,
		Files:         s.files,
		Parents:       s.parents,
		Children:      s.children,
		LinkCounts:    s.linkCounts,
		DeviceNumbers: s.deviceNumbers,
		PendingWrites: s.pendingWrites,
		Capabilities:  s.capabilities,
		StoreID:       s.storeID,
		ObjectIndex:   s.objectIndex,
	}

	// Acquire rollupMu and syncedMu to read rollupOffsets and synced
	// safely — these maps are governed by their own mutexes, not s.mu.
	// Shallow-copy into fresh maps so the snapshot does not alias the
	// live maps (rollupMu/syncedMu are released before gob encoding).
	s.rollupMu.RLock()
	if s.rollupOffsets != nil {
		ro := make(map[string]uint64, len(s.rollupOffsets))
		for k, v := range s.rollupOffsets {
			ro[k] = v
		}
		snap.RollupOffsets = ro
	}
	s.rollupMu.RUnlock()

	s.syncedMu.RLock()
	if s.synced != nil {
		sy := make(map[block.ContentHash]time.Time, len(s.synced))
		for k, v := range s.synced {
			sy[k] = v
		}
		snap.Synced = sy
	}
	s.syncedMu.RUnlock()

	// gob cannot encode map[string]any; pre-encode CustomSettings to JSON.
	snap.ServerConfig = s.serverConfig
	if len(s.serverConfig.CustomSettings) > 0 {
		csJSON, err := json.Marshal(s.serverConfig.CustomSettings)
		if err != nil {
			return nil, fmt.Errorf("%w: encode custom settings: %v", metadata.ErrBackupAborted, err)
		}
		snap.ServerConfigCustomSettingsJSON = csJSON
		// Nil out the unserializable field in the snapshot copy so gob
		// does not attempt to encode it.
		snap.ServerConfig.CustomSettings = nil
	}

	// Snapshot lazy sub-stores (nil if never initialized).
	if s.fileBlockData != nil {
		snap.FileBlockData = &fileBlockSnapshotData{
			Blocks:    s.fileBlockData.blocks,
			HashIndex: s.fileBlockData.hashIndex,
		}
	}
	if s.lockStore != nil {
		snap.Locks = &lockSnapshotData{
			Locks:       s.lockStore.locks,
			ServerEpoch: s.lockStore.serverEpoch,
		}
	}
	if s.clientStore != nil {
		snap.Clients = &clientSnapshotData{
			Registrations: s.clientStore.registrations,
		}
	}
	if s.durableStore != nil {
		snap.DurableHandles = &durableSnapshotData{
			Handles: s.durableStore.handles,
		}
	}

	// Extract every unique block hash into a HashSet.
	hs := block.NewHashSet(len(s.files))
	for _, fd := range s.files {
		if fd.Attr == nil {
			continue
		}
		for _, br := range fd.Attr.Blocks {
			hs.Add(br.Hash)
		}
	}

	// Write the envelope: header (magic + version + engine tag).
	envW, err := backup.NewWriter(w, memoryEngineTag)
	if err != nil {
		return nil, fmt.Errorf("%w: envelope header: %v", metadata.ErrBackupAborted, err)
	}

	// Write schema version (4-byte LE uint32).
	var vBuf [4]byte
	binary.LittleEndian.PutUint32(vBuf[:], memorySchemaVersion)
	if _, err := envW.Write(vBuf[:]); err != nil {
		return nil, fmt.Errorf("%w: schema version: %v", metadata.ErrBackupAborted, err)
	}

	// Gob-encode the snapshot into a temporary buffer so we can write
	// a length prefix. The envelope format has no payload-length field,
	// so the engine layer must frame the gob payload to prevent the gob
	// decoder's internal buffered reader from consuming the trailing CRC.
	var gobBuf bytes.Buffer
	enc := gob.NewEncoder(&gobBuf)
	if err := enc.Encode(&snap); err != nil {
		return nil, fmt.Errorf("%w: gob encode: %v", metadata.ErrBackupAborted, err)
	}

	// Write gob payload length (8-byte LE uint64).
	var lenBuf [8]byte
	binary.LittleEndian.PutUint64(lenBuf[:], uint64(gobBuf.Len()))
	if _, err := envW.Write(lenBuf[:]); err != nil {
		return nil, fmt.Errorf("%w: payload length: %v", metadata.ErrBackupAborted, err)
	}

	// Write the gob payload.
	if _, err := envW.Write(gobBuf.Bytes()); err != nil {
		return nil, fmt.Errorf("%w: payload write: %v", metadata.ErrBackupAborted, err)
	}

	// Trailing CRC.
	if err := envW.Finish(); err != nil {
		return nil, fmt.Errorf("%w: envelope finish: %v", metadata.ErrBackupAborted, err)
	}

	return hs, nil
}

// Restore reads a backup stream from r and rebuilds the store state.
// The destination store must be empty (no shares); otherwise
// ErrRestoreDestinationNotEmpty is returned.
func (s *MemoryMetadataStore) Restore(ctx context.Context, r io.Reader) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("restore cancelled: %w", err)
	}

	// Check destination is empty. Acquire read lock briefly.
	s.mu.RLock()
	hasShares := len(s.shares) > 0
	s.mu.RUnlock()

	if hasShares {
		return metadata.ErrRestoreDestinationNotEmpty
	}

	// Read and validate envelope header.
	engineTag, payloadReader, acc, err := backup.ReadHeader(r)
	if err != nil {
		return fmt.Errorf("%w: %v", metadata.ErrRestoreCorrupt, err)
	}

	// Verify engine tag.
	if err := backup.VerifyEngine(engineTag, memoryEngineTag); err != nil {
		return fmt.Errorf("%w: %v", metadata.ErrRestoreCorrupt, err)
	}

	// Read schema version (4 bytes LE).
	var vBuf [4]byte
	if _, err := io.ReadFull(payloadReader, vBuf[:]); err != nil {
		return fmt.Errorf("%w: read schema version: %v", metadata.ErrRestoreCorrupt, err)
	}
	schemaVersion := binary.LittleEndian.Uint32(vBuf[:])
	if schemaVersion != memorySchemaVersion {
		return fmt.Errorf("%w: got %d, want %d", metadata.ErrSchemaVersionMismatch, schemaVersion, memorySchemaVersion)
	}

	// Read gob payload length (8-byte LE uint64).
	var lenBuf [8]byte
	if _, err := io.ReadFull(payloadReader, lenBuf[:]); err != nil {
		return fmt.Errorf("%w: read payload length: %v", metadata.ErrRestoreCorrupt, err)
	}
	payloadLen := binary.LittleEndian.Uint64(lenBuf[:])

	// Reject oversized payload allocations from untrusted streams.
	if payloadLen > uint64(maxRestorePayloadSize) {
		return fmt.Errorf("%w: payload size %d exceeds maximum %d", metadata.ErrRestoreCorrupt, payloadLen, maxRestorePayloadSize)
	}

	// Read exactly payloadLen bytes into a buffer so the gob decoder's
	// internal buffered reader cannot consume the trailing CRC.
	gobData := make([]byte, payloadLen)
	if _, err := io.ReadFull(payloadReader, gobData); err != nil {
		return fmt.Errorf("%w: read payload: %v", metadata.ErrRestoreCorrupt, err)
	}

	// Gob-decode the snapshot.
	var snap memoryBackupSnapshot
	dec := gob.NewDecoder(bytes.NewReader(gobData))
	if err := dec.Decode(&snap); err != nil {
		return fmt.Errorf("%w: gob decode: %v", metadata.ErrRestoreCorrupt, err)
	}

	// Verify CRC using the ORIGINAL reader (not payloadReader).
	if err := backup.VerifyCRC(r, acc); err != nil {
		return fmt.Errorf("%w: %v", metadata.ErrRestoreCorrupt, err)
	}

	// Restore CustomSettings from JSON bytes.
	if len(snap.ServerConfigCustomSettingsJSON) > 0 {
		var cs map[string]any
		if err := json.Unmarshal(snap.ServerConfigCustomSettingsJSON, &cs); err != nil {
			return fmt.Errorf("%w: decode custom settings: %v", metadata.ErrRestoreCorrupt, err)
		}
		snap.ServerConfig.CustomSettings = cs
	}

	// Acquire write lock and populate all store fields.
	s.mu.Lock()
	defer s.mu.Unlock()

	s.shares = snap.Shares
	s.files = snap.Files
	s.parents = snap.Parents
	s.children = snap.Children
	s.linkCounts = snap.LinkCounts
	s.deviceNumbers = snap.DeviceNumbers
	s.pendingWrites = snap.PendingWrites
	s.serverConfig = snap.ServerConfig
	s.capabilities = snap.Capabilities
	s.storeID = snap.StoreID
	s.rollupMu.Lock()
	s.rollupOffsets = snap.RollupOffsets
	s.rollupMu.Unlock()

	s.syncedMu.Lock()
	s.synced = snap.Synced
	s.syncedMu.Unlock()

	s.objectIndex = snap.ObjectIndex

	// Ensure nil maps are initialized to empty (store invariant).
	if s.shares == nil {
		s.shares = make(map[string]*shareData)
	}
	if s.files == nil {
		s.files = make(map[string]*fileData)
	}
	if s.parents == nil {
		s.parents = make(map[string]metadata.FileHandle)
	}
	if s.children == nil {
		s.children = make(map[string]map[string]metadata.FileHandle)
	}
	if s.linkCounts == nil {
		s.linkCounts = make(map[string]uint32)
	}
	if s.deviceNumbers == nil {
		s.deviceNumbers = make(map[string]*deviceNumber)
	}
	if s.pendingWrites == nil {
		s.pendingWrites = make(map[string]*metadata.WriteOperation)
	}
	if s.rollupOffsets == nil {
		s.rollupOffsets = make(map[string]uint64)
	}
	if s.objectIndex == nil {
		s.objectIndex = make(map[block.ContentHash]string)
	}

	// Restore lazy sub-stores (nil snapshot = never initialized).
	// Explicit nil assignment in the else branch is required because the
	// destination store may have non-nil sub-stores from prior state.
	if snap.FileBlockData != nil {
		s.fileBlockData = &fileBlockStoreData{
			blocks:    snap.FileBlockData.Blocks,
			hashIndex: snap.FileBlockData.HashIndex,
		}
	} else {
		s.fileBlockData = nil
	}
	if snap.Locks != nil {
		s.lockStore = &memoryLockStore{
			locks:       snap.Locks.Locks,
			serverEpoch: snap.Locks.ServerEpoch,
		}
	} else {
		s.lockStore = nil
	}
	if snap.Clients != nil {
		s.clientStore = &memoryClientStore{
			registrations: snap.Clients.Registrations,
		}
	} else {
		s.clientStore = nil
	}
	if snap.DurableHandles != nil {
		s.durableStore = &memoryDurableStore{
			handles: snap.DurableHandles.Handles,
		}
	} else {
		s.durableStore = nil
	}

	// Re-initialize transient state.
	s.sessions = make(map[string]*metadata.ShareSession)

	// Recompute usedBytes from files (only regular files count).
	var totalBytes int64
	for _, fd := range s.files {
		if fd.Attr != nil && fd.Attr.Type == metadata.FileTypeRegular {
			totalBytes += int64(fd.Attr.Size)
		}
	}
	s.usedBytes.Store(totalBytes)

	return nil
}
