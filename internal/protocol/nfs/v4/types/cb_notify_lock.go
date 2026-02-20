// Package types - CB_NOTIFY_LOCK callback operation types (RFC 8881 Section 20.11).
//
// CB_NOTIFY_LOCK notifies the client that a previously denied lock may now
// be available. The client should retry the LOCK operation.
package types

import (
	"bytes"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/protocol/xdr"
)

// ============================================================================
// LockOwner4 - Lock Owner
// ============================================================================

// LockOwner4 represents lock_owner4 per RFC 8881 Section 2.5.
//
//	struct lock_owner4 {
//	    clientid4  clientid;
//	    opaque     owner<>;
//	};
type LockOwner4 struct {
	ClientID uint64
	Owner    []byte
}

// Encode writes the lock owner in XDR format.
func (lo *LockOwner4) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint64(buf, lo.ClientID); err != nil {
		return fmt.Errorf("encode lock_owner clientid: %w", err)
	}
	if err := xdr.WriteXDROpaque(buf, lo.Owner); err != nil {
		return fmt.Errorf("encode lock_owner owner: %w", err)
	}
	return nil
}

// Decode reads the lock owner from XDR format.
func (lo *LockOwner4) Decode(r io.Reader) error {
	var err error
	if lo.ClientID, err = xdr.DecodeUint64(r); err != nil {
		return fmt.Errorf("decode lock_owner clientid: %w", err)
	}
	if lo.Owner, err = xdr.DecodeOpaque(r); err != nil {
		return fmt.Errorf("decode lock_owner owner: %w", err)
	}
	return nil
}

// String returns a human-readable representation.
func (lo *LockOwner4) String() string {
	return fmt.Sprintf("LockOwner4{client=%d, owner=%x}", lo.ClientID, lo.Owner)
}

// ============================================================================
// CB_NOTIFY_LOCK4args (RFC 8881 Section 20.11.1)
// ============================================================================

// CbNotifyLockArgs represents CB_NOTIFY_LOCK4args per RFC 8881 Section 20.11.
//
//	struct CB_NOTIFY_LOCK4args {
//	    nfs_fh4      cnla_fh;
//	    lock_owner4  cnla_lock_owner;
//	};
type CbNotifyLockArgs struct {
	FH        []byte // filehandle
	LockOwner LockOwner4
}

// Encode writes the CB_NOTIFY_LOCK args in XDR format.
func (a *CbNotifyLockArgs) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteXDROpaque(buf, a.FH); err != nil {
		return fmt.Errorf("encode cb_notify_lock fh: %w", err)
	}
	if err := a.LockOwner.Encode(buf); err != nil {
		return fmt.Errorf("encode cb_notify_lock lock_owner: %w", err)
	}
	return nil
}

// Decode reads the CB_NOTIFY_LOCK args from XDR format.
func (a *CbNotifyLockArgs) Decode(r io.Reader) error {
	var err error
	if a.FH, err = xdr.DecodeOpaque(r); err != nil {
		return fmt.Errorf("decode cb_notify_lock fh: %w", err)
	}
	if err := a.LockOwner.Decode(r); err != nil {
		return fmt.Errorf("decode cb_notify_lock lock_owner: %w", err)
	}
	return nil
}

// String returns a human-readable representation.
func (a *CbNotifyLockArgs) String() string {
	return fmt.Sprintf("CbNotifyLockArgs{fh=%d bytes, owner=%s}",
		len(a.FH), a.LockOwner.String())
}

// ============================================================================
// CB_NOTIFY_LOCK4res (RFC 8881 Section 20.11.2)
// ============================================================================

// CbNotifyLockRes represents CB_NOTIFY_LOCK4res per RFC 8881 Section 20.11.
//
//	struct CB_NOTIFY_LOCK4res {
//	    nfsstat4 cnlr_status;
//	};
type CbNotifyLockRes struct {
	Status uint32
}

// Encode writes the CB_NOTIFY_LOCK result in XDR format.
func (res *CbNotifyLockRes) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint32(buf, res.Status); err != nil {
		return fmt.Errorf("encode cb_notify_lock status: %w", err)
	}
	return nil
}

// Decode reads the CB_NOTIFY_LOCK result from XDR format.
func (res *CbNotifyLockRes) Decode(r io.Reader) error {
	status, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode cb_notify_lock status: %w", err)
	}
	res.Status = status
	return nil
}

// String returns a human-readable representation.
func (res *CbNotifyLockRes) String() string {
	return fmt.Sprintf("CbNotifyLockRes{status=%d}", res.Status)
}
