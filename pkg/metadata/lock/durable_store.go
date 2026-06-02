package lock

import (
	"context"
	"time"
)

// PersistedDurableHandle is the storage representation of a durable open.
// When a client disconnects, the open file state is serialized into this struct
// and stored in the DurableHandleStore. If the client reconnects before the
// timeout expires, the state is restored and the open is resumed without data loss.
type PersistedDurableHandle struct {
	ID            string
	FileID        [16]byte
	Path          string
	ShareName     string
	DesiredAccess uint32
	// GrantedAccess is the DACL-evaluated per-bit mask returned to the
	// client at the original open time (MS-SMB2 §3.3.5.9 paragraph 8).
	// Persisted across durable reconnect so re-establishment of the open
	// (MS-SMB2 §3.3.5.9.7, §3.3.5.9.12) restores the exact granted set —
	// not a freshly re-resolved DesiredAccess, which would silently
	// inflate rights when the open was made with MAXIMUM_ALLOWED, or when
	// the file's DACL has changed since open. Mirrors Samba's
	// smbXsrv_open_global semantics where the access_mask field is
	// preserved verbatim across reconnect.
	GrantedAccess  uint32
	ShareAccess    uint32
	CreateOptions  uint32
	MetadataHandle []byte
	PayloadID      string
	OplockLevel    uint8
	LeaseKey       [16]byte
	LeaseState     uint32
	// LeaseEpoch is the SMB3 lease-V2 epoch (state-change counter,
	// MS-SMB2 2.2.13.2.8 / 2.2.23.2) captured at disconnect. Lives in the
	// protocol-agnostic lock layer as the durable counterpart to the live
	// OpLock.Lease.Epoch; persisted so a durable reconnect restores the
	// exact epoch the client last saw rather than resetting to 0. Without
	// it, the lease-response context on reconnect reports Epoch=0 and the
	// next break notification leaks a stale NewEpoch
	// (smb2.durable-v2-open.lock-lease asserts lease_epoch==1 on reconnect).
	// Pre-existing rows carry 0, which the reconnect handler treats as
	// "no persisted epoch" and falls back to the re-granted value.
	LeaseEpoch    uint16
	CreateGuid    [16]byte // V2 only; zero for V1
	AppInstanceId [16]byte // Zero if not set
	// IsPersistent records whether the durable open was granted as a
	// persistent handle (DH2Q SMB2_DHANDLE_FLAG_PERSISTENT) on a
	// continuous-availability share. Persisted so a reconnect re-echoes the
	// PERSISTENT flag in the DH2Q response (the open remains persistent across
	// the disconnect). Pre-existing rows decode false, which the reconnect
	// handler treats as a plain durable handle. The memory backend round-trips
	// this via struct copy and badger via JSON; postgres needs an explicit
	// column.
	IsPersistent    bool
	Username        string
	SessionKeyHash  [32]byte // SHA-256 hash, not raw key
	IsV2            bool
	CreatedAt       time.Time
	DisconnectedAt  time.Time
	TimeoutMs       uint32    // Handle expires at DisconnectedAt + TimeoutMs
	ServerStartTime time.Time // For timeout adjustment after server restart

	// Delete-on-close state for scavenger cleanup.
	// When DeletePending is true and the handle expires, the scavenger
	// deletes the file from the metadata store.
	DeletePending bool
	ParentHandle  []byte // Parent directory handle for deletion
	FileName      string // File name within parent for deletion
	IsDirectory   bool   // Whether the file is a directory

	// PositionInfo is the FILE_POSITION_INFORMATION CurrentByteOffset
	// (MS-FSCC 2.4.32) captured at disconnect. Restored to the OpenFile
	// on reconnect so SET/GET FilePositionInformation round-trips across
	// the disconnect (smb2.durable-open.file-position).
	PositionInfo uint64

	// RequestedAllocSize is the client-requested initial allocation in bytes
	// from the original CREATE's SMB2_CREATE_ALLOCATION_SIZE ("AlSi") create
	// context ([MS-SMB2] 2.2.13.2.2), or a later SET_INFO
	// FileAllocationInformation. DittoFS does not preallocate; the value only
	// raises the (cluster-aligned) AllocationSize reported in the CREATE
	// response. It is per-handle in-memory state lost on disconnect, so it is
	// persisted here and restored on durable reconnect — otherwise the
	// reconnect CREATE response would drop back to the file's bare size and
	// report a smaller AllocationSize than the original open
	// (smb2.durable-open.alloc-size reopen checks).
	RequestedAllocSize uint64

	// OriginalFileID is the full 16-byte FileID (persistent + volatile)
	// from the original CREATE response. FileID above zeros the volatile
	// half so DHnC lookup matches the spec ([MS-SMB2] 3.2.4.4: client
	// sends Data.Volatile=0). On successful reconnect the handler restores
	// OriginalFileID into the new OpenFile so byte-range locks (which key
	// on OpenID derived from FileID) stay valid across the disconnect.
	OriginalFileID [16]byte

	// ClientGUID is the 16-byte SMB2 client GUID from the NEGOTIATE that
	// established the original durable open. Per MS-SMB2 §3.3.5.9.12 and
	// Samba `smb2_lease_create` / lease-key scoping, V2 *lease-backed*
	// durable reconnect MUST verify the reconnecting connection's
	// ClientGUID matches; lease-keys are per-client-GUID, so a different
	// client must not be able to reclaim the lease. smbtorture
	// smb2.durable-v2-open.reopen1a-lease asserts that reconnect with a
	// new random ClientGUID fails OBJECT_NAME_NOT_FOUND while reconnect
	// with the original ClientGUID succeeds. Oplock-backed (non-lease) V2
	// durable reconnect is intentionally NOT gated on ClientGUID — the
	// `reopen1a` (oplock) test reconnects with a different ClientGUID and
	// expects success. V1 reconnect path does not consult this field.
	ClientGUID [16]byte
}

// DurableHandleStore provides persistence for SMB3 durable handle state.
// Implementations exist in memory, badger, and postgres stores.
//
// Reconnection flow:
//  1. On disconnect: persist open file state via PutDurableHandle
//  2. On reconnect: look up by FileID (V1) or CreateGuid (V2)
//  3. Validate security context and restore the open
//  4. Scavenger goroutine periodically calls DeleteExpiredDurableHandles
type DurableHandleStore interface {
	PutDurableHandle(ctx context.Context, handle *PersistedDurableHandle) error
	GetDurableHandle(ctx context.Context, id string) (*PersistedDurableHandle, error)
	GetDurableHandleByFileID(ctx context.Context, fileID [16]byte) (*PersistedDurableHandle, error)
	GetDurableHandleByCreateGuid(ctx context.Context, createGuid [16]byte) (*PersistedDurableHandle, error)
	// ConsumeDurableHandleByFileID atomically fetches and deletes the
	// persisted handle keyed by the original FileID, returning the previous
	// record (or nil if no match). Used on V1 reconnect (DHnC) to prevent
	// the TOCTOU window where two concurrent reconnects from the same
	// retrying client could both succeed a Get-then-Delete sequence and end
	// up with two live opens claiming the same persisted handle.
	// MS-SMB2 §3.3.5.9.7.
	ConsumeDurableHandleByFileID(ctx context.Context, fileID [16]byte) (*PersistedDurableHandle, error)
	// ConsumeDurableHandleByCreateGuid is the V2 (DH2C) counterpart of
	// ConsumeDurableHandleByFileID. MS-SMB2 §3.3.5.9.12.
	ConsumeDurableHandleByCreateGuid(ctx context.Context, createGuid [16]byte) (*PersistedDurableHandle, error)
	GetDurableHandlesByAppInstanceId(ctx context.Context, appInstanceId [16]byte) ([]*PersistedDurableHandle, error)
	GetDurableHandlesByFileHandle(ctx context.Context, fileHandle []byte) ([]*PersistedDurableHandle, error)
	DeleteDurableHandle(ctx context.Context, id string) error
	ListDurableHandles(ctx context.Context) ([]*PersistedDurableHandle, error)
	ListDurableHandlesByShare(ctx context.Context, shareName string) ([]*PersistedDurableHandle, error)
	DeleteExpiredDurableHandles(ctx context.Context, now time.Time) (int, error)
}
