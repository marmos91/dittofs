// Package types - CB_RECALL_ANY and CB_RECALLABLE_OBJ_AVAIL callback
// operation types (RFC 8881 Sections 20.6 and 20.7).
//
// CB_RECALL_ANY asks the client to return some recallable objects
// (delegations, layouts) to free server resources.
//
// CB_RECALLABLE_OBJ_AVAIL notifies the client that previously unavailable
// recallable objects are now available. It has void args and status-only res.
package types

import (
	"bytes"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/adapter/xdr"
)

// CB_RECALL_ANY type mask bit positions per RFC 8881 Section 20.6.
const (
	RCA4_TYPE_MASK_RDATA_DLG      = 0
	RCA4_TYPE_MASK_WDATA_DLG      = 1
	RCA4_TYPE_MASK_DIR_DLG        = 2
	RCA4_TYPE_MASK_FILE_LAYOUT    = 3
	RCA4_TYPE_MASK_BLK_LAYOUT     = 4
	RCA4_TYPE_MASK_OBJ_LAYOUT_MIN = 8
	RCA4_TYPE_MASK_OBJ_LAYOUT_MAX = 9
)

// ============================================================================
// CB_RECALL_ANY4args (RFC 8881 Section 20.6.1)
// ============================================================================

// CbRecallAnyArgs represents CB_RECALL_ANY4args per RFC 8881 Section 20.6.
//
//	struct CB_RECALL_ANY4args {
//	    uint32_t craa_objects_to_keep;
//	    bitmap4  craa_type_mask;
//	};
type CbRecallAnyArgs struct {
	ObjectsToKeep uint32
	TypeMask      Bitmap4
}

// Encode writes the CB_RECALL_ANY args in XDR format.
func (a *CbRecallAnyArgs) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint32(buf, a.ObjectsToKeep); err != nil {
		return fmt.Errorf("encode cb_recall_any objects_to_keep: %w", err)
	}
	if err := a.TypeMask.Encode(buf); err != nil {
		return fmt.Errorf("encode cb_recall_any type_mask: %w", err)
	}
	return nil
}

// Decode reads the CB_RECALL_ANY args from XDR format.
func (a *CbRecallAnyArgs) Decode(r io.Reader) error {
	var err error
	if a.ObjectsToKeep, err = xdr.DecodeUint32(r); err != nil {
		return fmt.Errorf("decode cb_recall_any objects_to_keep: %w", err)
	}
	if err := a.TypeMask.Decode(r); err != nil {
		return fmt.Errorf("decode cb_recall_any type_mask: %w", err)
	}
	return nil
}

// String returns a human-readable representation.
func (a *CbRecallAnyArgs) String() string {
	return fmt.Sprintf("CbRecallAnyArgs{keep=%d, mask=%s}", a.ObjectsToKeep, a.TypeMask.String())
}

// ============================================================================
// CB_RECALL_ANY4res (RFC 8881 Section 20.6.2)
// ============================================================================

// CbRecallAnyRes represents CB_RECALL_ANY4res per RFC 8881 Section 20.6.
//
//	struct CB_RECALL_ANY4res {
//	    nfsstat4 crar_status;
//	};
type CbRecallAnyRes struct {
	Status uint32
}

// Encode writes the CB_RECALL_ANY result in XDR format.
func (res *CbRecallAnyRes) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint32(buf, res.Status); err != nil {
		return fmt.Errorf("encode cb_recall_any status: %w", err)
	}
	return nil
}

// Decode reads the CB_RECALL_ANY result from XDR format.
func (res *CbRecallAnyRes) Decode(r io.Reader) error {
	status, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode cb_recall_any status: %w", err)
	}
	res.Status = status
	return nil
}

// String returns a human-readable representation.
func (res *CbRecallAnyRes) String() string {
	return fmt.Sprintf("CbRecallAnyRes{status=%d}", res.Status)
}

// ============================================================================
// CB_RECALLABLE_OBJ_AVAIL4args (RFC 8881 Section 20.7.1)
// ============================================================================

// CbRecallableObjAvailArgs represents CB_RECALLABLE_OBJ_AVAIL4args per RFC 8881 Section 20.7.
// This operation has void args (no fields).
//
//	struct CB_RECALLABLE_OBJ_AVAIL4args {
//	    void;
//	};
type CbRecallableObjAvailArgs struct{}

// Encode writes the CB_RECALLABLE_OBJ_AVAIL args in XDR format (void -- nothing to write).
func (a *CbRecallableObjAvailArgs) Encode(buf *bytes.Buffer) error {
	return nil
}

// Decode reads the CB_RECALLABLE_OBJ_AVAIL args from XDR format (void -- nothing to read).
func (a *CbRecallableObjAvailArgs) Decode(r io.Reader) error {
	return nil
}

// String returns a human-readable representation.
func (a *CbRecallableObjAvailArgs) String() string {
	return "CbRecallableObjAvailArgs{}"
}

// ============================================================================
// CB_RECALLABLE_OBJ_AVAIL4res (RFC 8881 Section 20.7.2)
// ============================================================================

// CbRecallableObjAvailRes represents CB_RECALLABLE_OBJ_AVAIL4res per RFC 8881 Section 20.7.
//
//	struct CB_RECALLABLE_OBJ_AVAIL4res {
//	    nfsstat4 croa_status;
//	};
type CbRecallableObjAvailRes struct {
	Status uint32
}

// Encode writes the CB_RECALLABLE_OBJ_AVAIL result in XDR format.
func (res *CbRecallableObjAvailRes) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint32(buf, res.Status); err != nil {
		return fmt.Errorf("encode cb_recallable_obj_avail status: %w", err)
	}
	return nil
}

// Decode reads the CB_RECALLABLE_OBJ_AVAIL result from XDR format.
func (res *CbRecallableObjAvailRes) Decode(r io.Reader) error {
	status, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode cb_recallable_obj_avail status: %w", err)
	}
	res.Status = status
	return nil
}

// String returns a human-readable representation.
func (res *CbRecallableObjAvailRes) String() string {
	return fmt.Sprintf("CbRecallableObjAvailRes{status=%d}", res.Status)
}
