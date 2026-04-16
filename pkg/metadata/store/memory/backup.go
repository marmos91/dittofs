package memory

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"runtime"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// Envelope layout (little-endian):
//
//	offset  size  field
//	------  ----  -----
//	0       4     magic "MDFS" (0x4d 0x44 0x46 0x53)
//	4       4     envelope FormatVersion (matches memoryFormatVersion)
//	8       8     payload length in bytes (uint64)
//	16      4     payload CRC32 IEEE (uint32)
//	20      N     gob-encoded memoryBackupRoot
//
// The envelope lets Restore reject a stream where:
//   - the magic was corrupted (random prefix → not ours)
//   - the payload length is wrong (truncation, concatenation)
//   - a bit flipped anywhere in the payload (CRC mismatch)
//
// Without this envelope, gob's self-describing framing tolerates enough
// bit flips to sneak structurally-valid but semantically-bogus data
// through Decode, violating T-02-02-01 (Corruption detection).
const (
	memoryBackupMagic          uint32 = 0x4d444653 // "MDFS"
	memoryBackupEnvelopeHeader        = 4 + 4 + 8 + 4
)

// memoryBackupCRCTable is the IEEE polynomial table reused across all
// Backup / Restore calls. crc32.IEEETable is a package-level sync.Once
// singleton already, but grabbing a local alias avoids a map lookup per
// byte on older compilers.
var memoryBackupCRCTable = crc32.IEEETable

// memoryBackupRoot is the gob-encoded top-level struct for Memory-store
// backups (D-05). Field ordering is part of the wire format — reordering
// or removing a field requires bumping GobSchemaVersion and is a
// breaking change.
//
// Format version history:
//
//	1: initial Phase-2 format (Phase 02-02)
//
// NOTE: sub-stores (memoryLockStore, memoryClientStore, memoryDurableStore,
// fileBlockStoreData) carry unexported fields that encoding/gob cannot reach.
// Rather than attach GobEncoder/GobDecoder methods to every sub-store (which
// would scatter backup concerns across files), the root struct captures the
// inner state directly as exported fields. On Restore, Close-free sub-stores
// are reconstituted from those exported fields and the usual lazy-init path
// continues to work.
type memoryBackupRoot struct {
	// Header (D-09)
	FormatVersion    uint32 // Phase-2-internal, starts at 1
	GobSchemaVersion uint32 // bumped when this struct changes
	GoVersion        string // runtime.Version() at backup time; advisory only

	// Core maps (D-01)
	Shares        map[string]*shareData
	Files         map[string]*fileData
	Parents       map[string]metadata.FileHandle
	Children      map[string]map[string]metadata.FileHandle
	LinkCounts    map[string]uint32
	DeviceNumbers map[string]*deviceNumber
	PendingWrites map[string]*metadata.WriteOperation

	// Value fields
	ServerConfig metadata.MetadataServerConfig
	Capabilities metadata.FilesystemCapabilities

	// Session state
	Sessions map[string]*metadata.ShareSession

	// Lazily-initialized sub-stores are captured as exported shadow fields
	// below. A non-nil shadow means the corresponding sub-store was initialized
	// at snapshot time and must be rebuilt on restore. All-nil shadows mean the
	// sub-store was never touched and restore leaves the pointer nil so the
	// existing lazy-init path continues to work untouched.

	// FileBlockBlocks / FileBlockHashIndex shadow fileBlockStoreData.
	HasFileBlockData   bool
	FileBlockBlocks    map[string]*metadata.FileBlock
	FileBlockHashIndex map[metadata.ContentHash]string

	// Lock* shadow memoryLockStore.
	HasLockStore    bool
	LockLocks       map[string]*lock.PersistedLock
	LockServerEpoch uint64

	// Client* shadow memoryClientStore.
	HasClientStore      bool
	ClientRegistrations map[string]*lock.PersistedClientRegistration

	// Durable* shadow memoryDurableStore.
	HasDurableStore bool
	DurableHandles  map[string]*lock.PersistedDurableHandle

	// UsedBytes is captured for audit; after Restore, recompute from Files
	// to guarantee consistency with actual file sizes.
	UsedBytes int64
}

const (
	memoryFormatVersion    uint32 = 1
	memoryGobSchemaVersion uint32 = 1
)

// Ensure MemoryMetadataStore implements metadata.Backupable.
var _ metadata.Backupable = (*MemoryMetadataStore)(nil)

// wrapAborted converts context cancellation / deadline errors into
// ErrBackupAborted, leaving other errors unwrapped by the sentinel. stage
// names which write produced the error so operator logs can locate the
// failing envelope component.
func wrapAborted(stage string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %v", metadata.ErrBackupAborted, err)
	}
	return fmt.Errorf("%w: %s: %v", metadata.ErrBackupAborted, stage, err)
}

// Backup streams a consistent snapshot of the store as a single gob-encoded
// memoryBackupRoot. The returned PayloadIDSet is computed under the SAME
// mu.RLock that wraps the encode (D-02) so the set and the payload stream
// reference exactly the same files.
func (store *MemoryMetadataStore) Backup(ctx context.Context, w io.Writer) (metadata.PayloadIDSet, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	store.mu.RLock()
	defer store.mu.RUnlock()

	// D-02: PayloadIDSet computed under the SAME RLock that wraps the encode.
	ids := metadata.NewPayloadIDSet()
	for _, fd := range store.files {
		if fd == nil || fd.Attr == nil {
			continue
		}
		if fd.Attr.PayloadID != "" {
			ids.Add(string(fd.Attr.PayloadID))
		}
	}

	root := memoryBackupRoot{
		FormatVersion:    memoryFormatVersion,
		GobSchemaVersion: memoryGobSchemaVersion,
		GoVersion:        runtime.Version(),

		Shares:        store.shares,
		Files:         store.files,
		Parents:       store.parents,
		Children:      store.children,
		LinkCounts:    store.linkCounts,
		DeviceNumbers: store.deviceNumbers,
		PendingWrites: store.pendingWrites,

		ServerConfig: store.serverConfig,
		Capabilities: store.capabilities,

		Sessions: store.sessions,

		UsedBytes: store.usedBytes.Load(),
	}

	// Capture lazy sub-store inner state. Each sub-store has its own RWMutex
	// independent of store.mu — acquire each one's RLock and shallow-clone the
	// map so the gob encoder iterates a private copy. Aliasing the live map
	// would race with any concurrent mutator that holds only the sub-store
	// lock (PutLock, PutClientRegistration, PutDurableHandle).
	if store.fileBlockData != nil {
		root.HasFileBlockData = true
		root.FileBlockBlocks = cloneMap(store.fileBlockData.blocks)
		root.FileBlockHashIndex = cloneMap(store.fileBlockData.hashIndex)
	}
	if store.lockStore != nil {
		store.lockStore.mu.RLock()
		root.HasLockStore = true
		root.LockLocks = cloneMap(store.lockStore.locks)
		root.LockServerEpoch = store.lockStore.serverEpoch
		store.lockStore.mu.RUnlock()
	}
	if store.clientStore != nil {
		store.clientStore.mu.RLock()
		root.HasClientStore = true
		root.ClientRegistrations = cloneMap(store.clientStore.registrations)
		store.clientStore.mu.RUnlock()
	}
	if store.durableStore != nil {
		store.durableStore.mu.RLock()
		root.HasDurableStore = true
		root.DurableHandles = cloneMap(store.durableStore.handles)
		store.durableStore.mu.RUnlock()
	}

	// Encode the payload into an in-memory buffer first so we can compute
	// its length + CRC32 for the envelope header. Memory store sizes are
	// small by design (this driver is the parity/test canary — see D-05).
	var payload bytes.Buffer
	if err := gob.NewEncoder(&payload).Encode(&root); err != nil {
		return nil, wrapAborted("gob encode", err)
	}

	header := make([]byte, memoryBackupEnvelopeHeader)
	binary.LittleEndian.PutUint32(header[0:4], memoryBackupMagic)
	binary.LittleEndian.PutUint32(header[4:8], memoryFormatVersion)
	binary.LittleEndian.PutUint64(header[8:16], uint64(payload.Len()))
	binary.LittleEndian.PutUint32(header[16:20], crc32.Checksum(payload.Bytes(), memoryBackupCRCTable))

	if _, err := w.Write(header); err != nil {
		return nil, wrapAborted("envelope header", err)
	}
	if _, err := w.Write(payload.Bytes()); err != nil {
		return nil, wrapAborted("envelope payload", err)
	}

	return ids, nil
}

// Restore reloads the store from r. The store MUST be empty (no shares, no
// files) — otherwise ErrRestoreDestinationNotEmpty is returned before any
// state is touched (D-06). Decode errors are wrapped with ErrRestoreCorrupt.
// After a successful restore, usedBytes is recomputed from Files so an
// archive whose UsedBytes field was tampered with cannot drive a quota-evasion
// via restore (T-02-02-06).
func (store *MemoryMetadataStore) Restore(ctx context.Context, r io.Reader) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	if len(store.files) > 0 || len(store.shares) > 0 {
		return metadata.ErrRestoreDestinationNotEmpty
	}

	// Parse the envelope header. Short reads here (EOF / unexpected EOF) are
	// corruption — the stream was truncated below the header size.
	header := make([]byte, memoryBackupEnvelopeHeader)
	if _, err := io.ReadFull(r, header); err != nil {
		return fmt.Errorf("%w: read envelope header: %v", metadata.ErrRestoreCorrupt, err)
	}
	magic := binary.LittleEndian.Uint32(header[0:4])
	if magic != memoryBackupMagic {
		return fmt.Errorf("%w: invalid magic 0x%08x (want 0x%08x)",
			metadata.ErrRestoreCorrupt, magic, memoryBackupMagic)
	}
	envelopeVersion := binary.LittleEndian.Uint32(header[4:8])
	if envelopeVersion == 0 || envelopeVersion > memoryFormatVersion {
		return fmt.Errorf("%w: unsupported envelope FormatVersion=%d",
			metadata.ErrRestoreCorrupt, envelopeVersion)
	}
	payloadLen := binary.LittleEndian.Uint64(header[8:16])
	// Reject absurd lengths — 1 GiB is well above any plausible in-memory
	// metadata store. Bounding here prevents a tampered archive from
	// allocating a multi-GB buffer (T-02-02-04 DoS mitigation).
	const maxPayload uint64 = 1 << 30
	if payloadLen > maxPayload {
		return fmt.Errorf("%w: payload length %d exceeds limit %d",
			metadata.ErrRestoreCorrupt, payloadLen, maxPayload)
	}
	expectedCRC := binary.LittleEndian.Uint32(header[16:20])

	payload := make([]byte, payloadLen)
	if _, err := io.ReadFull(r, payload); err != nil {
		return fmt.Errorf("%w: read envelope payload: %v", metadata.ErrRestoreCorrupt, err)
	}
	if got := crc32.Checksum(payload, memoryBackupCRCTable); got != expectedCRC {
		return fmt.Errorf("%w: payload CRC mismatch (got 0x%08x, want 0x%08x)",
			metadata.ErrRestoreCorrupt, got, expectedCRC)
	}

	var root memoryBackupRoot
	if err := gob.NewDecoder(bytes.NewReader(payload)).Decode(&root); err != nil {
		return fmt.Errorf("%w: %v", metadata.ErrRestoreCorrupt, err)
	}

	// Reject archives from an unknown / newer format version. FormatVersion==0
	// guards against an entirely empty / zero-valued stream that happened to
	// gob-decode successfully (all-zero fields). FormatVersion > current
	// rejects forward-incompatible archives.
	if root.FormatVersion == 0 || root.FormatVersion > memoryFormatVersion {
		return fmt.Errorf("%w: unsupported memory backup format_version=%d",
			metadata.ErrRestoreCorrupt, root.FormatVersion)
	}
	if root.GobSchemaVersion == 0 || root.GobSchemaVersion > memoryGobSchemaVersion {
		return fmt.Errorf("%w: unsupported memory backup gob_schema_version=%d",
			metadata.ErrRestoreCorrupt, root.GobSchemaVersion)
	}

	// Replace internals atomically. nil-safe for every map so the restored
	// store is fully usable even if the archive's maps were nil (e.g. an
	// empty source store).
	store.shares = nilSafeMap(root.Shares)
	store.files = nilSafeMap(root.Files)
	store.parents = nilSafeMap(root.Parents)
	store.children = nilSafeMap(root.Children)
	store.linkCounts = nilSafeMap(root.LinkCounts)
	store.deviceNumbers = nilSafeMap(root.DeviceNumbers)
	store.pendingWrites = nilSafeMap(root.PendingWrites)
	store.serverConfig = root.ServerConfig
	store.capabilities = root.Capabilities
	store.sessions = nilSafeMap(root.Sessions)

	// Reconstitute lazy sub-stores from the captured inner state. If a
	// shadow field was not set, leave the pointer nil so the existing
	// lazy-init path runs on first use.
	if root.HasFileBlockData {
		store.fileBlockData = &fileBlockStoreData{
			blocks:    nilSafeMap(root.FileBlockBlocks),
			hashIndex: nilSafeMap(root.FileBlockHashIndex),
		}
	} else {
		store.fileBlockData = nil
	}
	if root.HasLockStore {
		store.lockStore = &memoryLockStore{
			locks:       nilSafeMap(root.LockLocks),
			serverEpoch: root.LockServerEpoch,
		}
	} else {
		store.lockStore = nil
	}
	if root.HasClientStore {
		store.clientStore = &memoryClientStore{
			registrations: nilSafeMap(root.ClientRegistrations),
		}
	} else {
		store.clientStore = nil
	}
	if root.HasDurableStore {
		store.durableStore = &memoryDurableStore{
			handles: nilSafeMap(root.DurableHandles),
		}
	} else {
		store.durableStore = nil
	}

	// Rebuild sortedDirCache lazily — the cache is derivable from children
	// and is never persisted (D-01).
	store.sortedDirCache = make(map[string][]string)

	// Recompute usedBytes from the restored Files — don't trust the archive
	// value blindly (T-02-02-06 defense in depth).
	var used int64
	for _, fd := range store.files {
		if fd == nil || fd.Attr == nil {
			continue
		}
		if fd.Attr.Type == metadata.FileTypeRegular {
			used += int64(fd.Attr.Size)
		}
	}
	store.usedBytes.Store(used)

	return nil
}

// nilSafeMap returns m if non-nil, else an empty map of the same type. gob
// decode leaves maps nil when the encoded value was nil, but the rest of the
// memory store expects non-nil maps to iterate over.
func nilSafeMap[K comparable, V any](m map[K]V) map[K]V {
	if m == nil {
		return make(map[K]V)
	}
	return m
}

// cloneMap returns a shallow copy of m. Phase 2 Backup uses this to isolate
// the gob encoder from live maps protected by sub-store locks we release
// before encoding — aliasing would race with concurrent mutators.
func cloneMap[K comparable, V any](m map[K]V) map[K]V {
	if m == nil {
		return nil
	}
	out := make(map[K]V, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
