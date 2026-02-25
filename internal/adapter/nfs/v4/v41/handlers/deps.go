// Package v41handlers contains NFSv4.1-only operation handlers.
// These handlers were extracted from the v4/handlers package to separate
// v4.1-specific operations from shared v4.0/v4.1 infrastructure.
package v41handlers

import (
	"encoding/binary"
	"errors"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/state"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// Deps holds shared dependencies for v4.1 handlers.
type Deps struct {
	// StateManager is the central NFSv4 state coordinator.
	StateManager *state.StateManager
}

// EncodeStatusOnly XDR-encodes a status-only response (just the nfsstat4).
func EncodeStatusOnly(status uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, status)
	return b
}

// MapStateError converts a state-layer error to an NFS status code.
func MapStateError(err error) uint32 {
	var stateErr *state.NFS4StateError
	if errors.As(err, &stateErr) {
		return stateErr.Status
	}
	switch {
	case errors.Is(err, state.ErrStaleClientID):
		return types.NFS4ERR_STALE_CLIENTID
	case errors.Is(err, state.ErrClientIDInUse):
		return types.NFS4ERR_CLID_INUSE
	default:
		return types.NFS4ERR_SERVERFAULT
	}
}
