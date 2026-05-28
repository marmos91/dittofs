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
	GrantedAccess   uint32
	ShareAccess     uint32
	CreateOptions   uint32
	MetadataHandle  []byte
	PayloadID       string
	OplockLevel     uint8
	LeaseKey        [16]byte
	LeaseState      uint32
	CreateGuid      [16]byte // V2 only; zero for V1
	AppInstanceId   [16]byte // Zero if not set
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

	// OriginalFileID is the full 16-byte FileID (persistent + volatile)
	// from the original CREATE response. FileID above zeros the volatile
	// half so DHnC lookup matches the spec ([MS-SMB2] 3.2.4.4: client
	// sends Data.Volatile=0). On successful reconnect the handler restores
	// OriginalFileID into the new OpenFile so byte-range locks (which key
	// on OpenID derived from FileID) stay valid across the disconnect.
	OriginalFileID [16]byte
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
	GetDurableHandlesByAppInstanceId(ctx context.Context, appInstanceId [16]byte) ([]*PersistedDurableHandle, error)
	GetDurableHandlesByFileHandle(ctx context.Context, fileHandle []byte) ([]*PersistedDurableHandle, error)
	DeleteDurableHandle(ctx context.Context, id string) error
	ListDurableHandles(ctx context.Context) ([]*PersistedDurableHandle, error)
	ListDurableHandlesByShare(ctx context.Context, shareName string) ([]*PersistedDurableHandle, error)
	DeleteExpiredDurableHandles(ctx context.Context, now time.Time) (int, error)
}
