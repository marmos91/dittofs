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
	ID              string
	FileID          [16]byte
	Path            string
	ShareName       string
	DesiredAccess   uint32
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
