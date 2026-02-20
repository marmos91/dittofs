// Package types - CB_NOTIFY callback operation types (RFC 8881 Section 20.4).
//
// CB_NOTIFY sends directory change notifications to clients holding directory
// delegations. Each notification carries a bitmap of change types and opaque
// notification entries whose format depends on the notification type.
// Full parsing of notification sub-types (notify_add4, notify_remove4, etc.)
// will be done in Phase 24 (directory delegations).
package types

import (
	"bytes"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/protocol/xdr"
)

// ============================================================================
// Notify4 / NotifyEntry4 - Notification structures
// ============================================================================

// NotifyEntry4 represents a single opaque notification entry.
// The actual format depends on the notification type (NOTIFY4_ADD_ENTRY,
// NOTIFY4_REMOVE_ENTRY, etc.) and will be parsed in Phase 24.
type NotifyEntry4 struct {
	Data []byte // raw opaque entry data
}

// Notify4 represents a notify4 structure per RFC 8881 Section 20.4.
//
//	struct notify4 {
//	    bitmap4         notify_mask;
//	    notify_entry4   notify_vals<>;
//	};
type Notify4 struct {
	Mask   Bitmap4        // notification types (NOTIFY4_* constants)
	Values []NotifyEntry4 // variable-length array of entries
}

// Encode writes the Notify4 in XDR format.
func (n *Notify4) Encode(buf *bytes.Buffer) error {
	if err := n.Mask.Encode(buf); err != nil {
		return fmt.Errorf("encode notify mask: %w", err)
	}
	count := uint32(len(n.Values))
	if err := xdr.WriteUint32(buf, count); err != nil {
		return fmt.Errorf("encode notify values count: %w", err)
	}
	for i := range n.Values {
		if err := xdr.WriteXDROpaque(buf, n.Values[i].Data); err != nil {
			return fmt.Errorf("encode notify_entry[%d]: %w", i, err)
		}
	}
	return nil
}

// Decode reads the Notify4 from XDR format.
func (n *Notify4) Decode(r io.Reader) error {
	if err := n.Mask.Decode(r); err != nil {
		return fmt.Errorf("decode notify mask: %w", err)
	}
	count, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode notify values count: %w", err)
	}
	if count > 4096 {
		return fmt.Errorf("notify values count %d exceeds limit", count)
	}
	n.Values = make([]NotifyEntry4, count)
	for i := uint32(0); i < count; i++ {
		data, err := xdr.DecodeOpaque(r)
		if err != nil {
			return fmt.Errorf("decode notify_entry[%d]: %w", i, err)
		}
		n.Values[i].Data = data
	}
	return nil
}

// String returns a human-readable representation.
func (n *Notify4) String() string {
	return fmt.Sprintf("Notify4{mask=%s, entries=%d}", n.Mask.String(), len(n.Values))
}

// ============================================================================
// CB_NOTIFY4args (RFC 8881 Section 20.4.1)
// ============================================================================

// CbNotifyArgs represents CB_NOTIFY4args per RFC 8881 Section 20.4.
//
//	struct CB_NOTIFY4args {
//	    stateid4    cna_stateid;
//	    nfs_fh4     cna_fh;
//	    notify4     cna_changes<>;
//	};
type CbNotifyArgs struct {
	Stateid Stateid4  // directory delegation stateid
	FH      []byte    // filehandle of directory
	Changes []Notify4 // variable-length array of notification entries
}

// Encode writes the CB_NOTIFY args in XDR format.
func (a *CbNotifyArgs) Encode(buf *bytes.Buffer) error {
	EncodeStateid4(buf, &a.Stateid)
	if err := xdr.WriteXDROpaque(buf, a.FH); err != nil {
		return fmt.Errorf("encode cb_notify fh: %w", err)
	}
	count := uint32(len(a.Changes))
	if err := xdr.WriteUint32(buf, count); err != nil {
		return fmt.Errorf("encode cb_notify changes count: %w", err)
	}
	for i := range a.Changes {
		if err := a.Changes[i].Encode(buf); err != nil {
			return fmt.Errorf("encode cb_notify change[%d]: %w", i, err)
		}
	}
	return nil
}

// Decode reads the CB_NOTIFY args from XDR format.
func (a *CbNotifyArgs) Decode(r io.Reader) error {
	sid, err := DecodeStateid4(r)
	if err != nil {
		return fmt.Errorf("decode cb_notify stateid: %w", err)
	}
	a.Stateid = *sid
	if a.FH, err = xdr.DecodeOpaque(r); err != nil {
		return fmt.Errorf("decode cb_notify fh: %w", err)
	}
	count, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode cb_notify changes count: %w", err)
	}
	if count > 4096 {
		return fmt.Errorf("cb_notify changes count %d exceeds limit", count)
	}
	a.Changes = make([]Notify4, count)
	for i := uint32(0); i < count; i++ {
		if err := a.Changes[i].Decode(r); err != nil {
			return fmt.Errorf("decode cb_notify change[%d]: %w", i, err)
		}
	}
	return nil
}

// String returns a human-readable representation.
func (a *CbNotifyArgs) String() string {
	return fmt.Sprintf("CbNotifyArgs{stateid={seq=%d}, fh=%d bytes, changes=%d}",
		a.Stateid.Seqid, len(a.FH), len(a.Changes))
}

// ============================================================================
// CB_NOTIFY4res (RFC 8881 Section 20.4.2)
// ============================================================================

// CbNotifyRes represents CB_NOTIFY4res per RFC 8881 Section 20.4.
//
//	struct CB_NOTIFY4res {
//	    nfsstat4 cnr_status;
//	};
type CbNotifyRes struct {
	Status uint32
}

// Encode writes the CB_NOTIFY result in XDR format.
func (res *CbNotifyRes) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint32(buf, res.Status); err != nil {
		return fmt.Errorf("encode cb_notify status: %w", err)
	}
	return nil
}

// Decode reads the CB_NOTIFY result from XDR format.
func (res *CbNotifyRes) Decode(r io.Reader) error {
	status, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode cb_notify status: %w", err)
	}
	res.Status = status
	return nil
}

// String returns a human-readable representation.
func (res *CbNotifyRes) String() string {
	return fmt.Sprintf("CbNotifyRes{status=%d}", res.Status)
}
