// Package types - CB_NOTIFY callback operation types (RFC 8881 Section 20.4).
//
// CB_NOTIFY sends directory change notifications to clients holding directory
// delegations. Each notification carries a bitmap of change types and opaque
// notification entries whose format depends on the notification type.
//
// Sub-type encoders (NotifyAdd4, NotifyRemove4, NotifyRename4,
// NotifyAttrChange4) produce the inner opaque data for each notification
// type within the notify4 structure per RFC 8881 Section 20.4.
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
// The format depends on the notification type (NOTIFY4_ADD_ENTRY,
// NOTIFY4_REMOVE_ENTRY, etc.) and is encoded by the sub-type encoders below.
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
	for i := range n.Values {
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
	for i := range a.Changes {
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

// ============================================================================
// CB_NOTIFY Sub-Type Encoders (RFC 8881 Section 20.4)
// ============================================================================

// NotifyAdd4 represents a notify_add4 entry per RFC 8881.
// Encodes an ADD_ENTRY notification when a new directory entry is created.
//
//	struct notify_add4 {
//	    notify_entry4_pair  nad_new_entry;
//	    /* what was added */
//	    notify_entry4_pair  nad_old_entry<1>;
//	    /* what it was added beside */
//	};
//
//	struct notify_entry4_pair {
//	    component4  ne_name;
//	    nfs_cookie4 ne_cookie;
//	    fattr4      ne_attrs;
//	};
type NotifyAdd4 struct {
	EntryName  string // component4: name of the new entry
	Cookie     uint64 // nfs_cookie4: readdir cookie for the entry
	Attrs      []byte // pre-encoded fattr4 bytes (may be empty)
	HasPrev    bool   // whether a previous entry hint is included
	PrevName   string // component4: name of the predecessor entry
	PrevCookie uint64 // nfs_cookie4: cookie of the predecessor entry
}

// Encode writes the NotifyAdd4 in XDR format.
func (n *NotifyAdd4) Encode(buf *bytes.Buffer) error {
	// nad_new_entry: notify_entry4_pair
	if err := xdr.WriteXDRString(buf, n.EntryName); err != nil {
		return fmt.Errorf("encode add entry name: %w", err)
	}
	if err := xdr.WriteUint64(buf, n.Cookie); err != nil {
		return fmt.Errorf("encode add entry cookie: %w", err)
	}
	// fattr4: bitmap4 + opaque attrlist
	if len(n.Attrs) > 0 {
		_, _ = buf.Write(n.Attrs)
	} else {
		// Empty fattr4: bitmap count=0, attrlist length=0
		_ = xdr.WriteUint32(buf, 0) // bitmap count
		_ = xdr.WriteUint32(buf, 0) // attrlist length
	}

	// nad_old_entry<1>: optional array
	if n.HasPrev {
		_ = xdr.WriteUint32(buf, 1) // array count = 1
		if err := xdr.WriteXDRString(buf, n.PrevName); err != nil {
			return fmt.Errorf("encode add prev name: %w", err)
		}
		if err := xdr.WriteUint64(buf, n.PrevCookie); err != nil {
			return fmt.Errorf("encode add prev cookie: %w", err)
		}
		// Empty fattr4 for prev entry
		_ = xdr.WriteUint32(buf, 0) // bitmap count
		_ = xdr.WriteUint32(buf, 0) // attrlist length
	} else {
		_ = xdr.WriteUint32(buf, 0) // array count = 0
	}
	return nil
}

// NotifyRemove4 represents a notify_remove4 entry per RFC 8881.
// Encodes a REMOVE_ENTRY notification when a directory entry is deleted.
//
//	struct notify_remove4 {
//	    notify_entry4_pair  nrm_old_entry;
//	};
type NotifyRemove4 struct {
	EntryName string // component4: name of the removed entry
	Cookie    uint64 // nfs_cookie4: readdir cookie for the entry
}

// Encode writes the NotifyRemove4 in XDR format.
func (n *NotifyRemove4) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteXDRString(buf, n.EntryName); err != nil {
		return fmt.Errorf("encode remove entry name: %w", err)
	}
	if err := xdr.WriteUint64(buf, n.Cookie); err != nil {
		return fmt.Errorf("encode remove entry cookie: %w", err)
	}
	// Empty fattr4 for removed entry
	_ = xdr.WriteUint32(buf, 0) // bitmap count
	_ = xdr.WriteUint32(buf, 0) // attrlist length
	return nil
}

// NotifyRename4 represents a notify_rename4 entry per RFC 8881.
// Encodes a RENAME_ENTRY notification as a single RENAME event.
//
//	struct notify_rename4 {
//	    notify_entry4_pair  nrn_old_entry;
//	    notify_entry4_pair  nrn_new_entry;
//	};
type NotifyRename4 struct {
	OldEntryName string // component4: old name
	NewEntryName string // component4: new name
}

// Encode writes the NotifyRename4 in XDR format.
func (n *NotifyRename4) Encode(buf *bytes.Buffer) error {
	// nrn_old_entry
	if err := xdr.WriteXDRString(buf, n.OldEntryName); err != nil {
		return fmt.Errorf("encode rename old name: %w", err)
	}
	_ = xdr.WriteUint64(buf, 0) // cookie (zero for old entry)
	_ = xdr.WriteUint32(buf, 0) // empty fattr4 bitmap count
	_ = xdr.WriteUint32(buf, 0) // empty fattr4 attrlist length

	// nrn_new_entry
	if err := xdr.WriteXDRString(buf, n.NewEntryName); err != nil {
		return fmt.Errorf("encode rename new name: %w", err)
	}
	_ = xdr.WriteUint64(buf, 0) // cookie (zero for new entry)
	_ = xdr.WriteUint32(buf, 0) // empty fattr4 bitmap count
	_ = xdr.WriteUint32(buf, 0) // empty fattr4 attrlist length
	return nil
}

// NotifyAttrChange4 represents a notify_attr_change4 entry per RFC 8881.
// Encodes a CHANGE_CHILD_ATTRS notification when a child entry's attributes change.
//
//	struct notify_attr_change4 {
//	    component4  nac_name;
//	    nfs_cookie4 nac_cookie;
//	    fattr4      nac_new_attrs;
//	};
type NotifyAttrChange4 struct {
	EntryName string // component4: name of the entry whose attrs changed
	Cookie    uint64 // nfs_cookie4: readdir cookie
	Attrs     []byte // pre-encoded fattr4 bytes (must be non-empty for attr changes)
}

// Encode writes the NotifyAttrChange4 in XDR format.
func (n *NotifyAttrChange4) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteXDRString(buf, n.EntryName); err != nil {
		return fmt.Errorf("encode attr_change entry name: %w", err)
	}
	if err := xdr.WriteUint64(buf, n.Cookie); err != nil {
		return fmt.Errorf("encode attr_change cookie: %w", err)
	}
	if len(n.Attrs) > 0 {
		_, _ = buf.Write(n.Attrs)
	} else {
		// Empty fattr4
		_ = xdr.WriteUint32(buf, 0) // bitmap count
		_ = xdr.WriteUint32(buf, 0) // attrlist length
	}
	return nil
}
