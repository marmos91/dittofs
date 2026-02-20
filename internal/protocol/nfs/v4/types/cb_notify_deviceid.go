// Package types - CB_NOTIFY_DEVICEID callback operation types (RFC 8881 Section 20.12).
//
// CB_NOTIFY_DEVICEID notifies the client about changes to device IDs used in
// pNFS layouts. A device ID may be changed (address updated) or deleted.
// Included for wire compatibility; pNFS is out of scope for v3.0.
package types

import (
	"bytes"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/protocol/xdr"
)

// Device ID notification types per RFC 8881 Section 20.12.
const (
	NOTIFY_DEVICEID4_CHANGE = 1
	NOTIFY_DEVICEID4_DELETE = 2
)

// ============================================================================
// NotifyDeviceIdChange4 - Device ID change entry
// ============================================================================

// NotifyDeviceIdChange4 represents a single device ID change notification.
//
//	struct notify_deviceid_change4 {
//	    notify_deviceid_type4  ndc_change_type;
//	    deviceid4              ndc_deviceid;
//	    bool                   ndc_immediate; // only for CHANGE
//	};
//
// For DELETE, ndc_immediate is not present on the wire. We use the Type
// discriminant to decide encoding.
type NotifyDeviceIdChange4 struct {
	Type      uint32   // NOTIFY_DEVICEID4_CHANGE or NOTIFY_DEVICEID4_DELETE
	DeviceID  DeviceId4
	Immediate bool // only meaningful for NOTIFY_DEVICEID4_CHANGE
}

// Encode writes the device ID change entry in XDR format.
func (n *NotifyDeviceIdChange4) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint32(buf, n.Type); err != nil {
		return fmt.Errorf("encode notify_deviceid type: %w", err)
	}
	if _, err := buf.Write(n.DeviceID[:]); err != nil {
		return fmt.Errorf("encode notify_deviceid device_id: %w", err)
	}
	if n.Type == NOTIFY_DEVICEID4_CHANGE {
		if err := xdr.WriteBool(buf, n.Immediate); err != nil {
			return fmt.Errorf("encode notify_deviceid immediate: %w", err)
		}
	}
	return nil
}

// Decode reads the device ID change entry from XDR format.
func (n *NotifyDeviceIdChange4) Decode(r io.Reader) error {
	var err error
	if n.Type, err = xdr.DecodeUint32(r); err != nil {
		return fmt.Errorf("decode notify_deviceid type: %w", err)
	}
	if _, err := io.ReadFull(r, n.DeviceID[:]); err != nil {
		return fmt.Errorf("decode notify_deviceid device_id: %w", err)
	}
	if n.Type == NOTIFY_DEVICEID4_CHANGE {
		if n.Immediate, err = xdr.DecodeBool(r); err != nil {
			return fmt.Errorf("decode notify_deviceid immediate: %w", err)
		}
	}
	return nil
}

// String returns a human-readable representation.
func (n *NotifyDeviceIdChange4) String() string {
	typeName := "CHANGE"
	if n.Type == NOTIFY_DEVICEID4_DELETE {
		typeName = "DELETE"
	}
	return fmt.Sprintf("NotifyDeviceIdChange4{type=%s, device=%x}", typeName, n.DeviceID)
}

// ============================================================================
// CB_NOTIFY_DEVICEID4args (RFC 8881 Section 20.12.1)
// ============================================================================

// CbNotifyDeviceidArgs represents CB_NOTIFY_DEVICEID4args per RFC 8881 Section 20.12.
//
//	struct CB_NOTIFY_DEVICEID4args {
//	    notify_deviceid_change4 cnda_changes<>;
//	};
type CbNotifyDeviceidArgs struct {
	Changes []NotifyDeviceIdChange4
}

// Encode writes the CB_NOTIFY_DEVICEID args in XDR format.
func (a *CbNotifyDeviceidArgs) Encode(buf *bytes.Buffer) error {
	count := uint32(len(a.Changes))
	if err := xdr.WriteUint32(buf, count); err != nil {
		return fmt.Errorf("encode cb_notify_deviceid changes count: %w", err)
	}
	for i := range a.Changes {
		if err := a.Changes[i].Encode(buf); err != nil {
			return fmt.Errorf("encode cb_notify_deviceid change[%d]: %w", i, err)
		}
	}
	return nil
}

// Decode reads the CB_NOTIFY_DEVICEID args from XDR format.
func (a *CbNotifyDeviceidArgs) Decode(r io.Reader) error {
	count, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode cb_notify_deviceid changes count: %w", err)
	}
	if count > 4096 {
		return fmt.Errorf("cb_notify_deviceid changes count %d exceeds limit", count)
	}
	a.Changes = make([]NotifyDeviceIdChange4, count)
	for i := uint32(0); i < count; i++ {
		if err := a.Changes[i].Decode(r); err != nil {
			return fmt.Errorf("decode cb_notify_deviceid change[%d]: %w", i, err)
		}
	}
	return nil
}

// String returns a human-readable representation.
func (a *CbNotifyDeviceidArgs) String() string {
	return fmt.Sprintf("CbNotifyDeviceidArgs{changes=%d}", len(a.Changes))
}

// ============================================================================
// CB_NOTIFY_DEVICEID4res (RFC 8881 Section 20.12.2)
// ============================================================================

// CbNotifyDeviceidRes represents CB_NOTIFY_DEVICEID4res per RFC 8881 Section 20.12.
//
//	struct CB_NOTIFY_DEVICEID4res {
//	    nfsstat4 cndr_status;
//	};
type CbNotifyDeviceidRes struct {
	Status uint32
}

// Encode writes the CB_NOTIFY_DEVICEID result in XDR format.
func (res *CbNotifyDeviceidRes) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint32(buf, res.Status); err != nil {
		return fmt.Errorf("encode cb_notify_deviceid status: %w", err)
	}
	return nil
}

// Decode reads the CB_NOTIFY_DEVICEID result from XDR format.
func (res *CbNotifyDeviceidRes) Decode(r io.Reader) error {
	status, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode cb_notify_deviceid status: %w", err)
	}
	res.Status = status
	return nil
}

// String returns a human-readable representation.
func (res *CbNotifyDeviceidRes) String() string {
	return fmt.Sprintf("CbNotifyDeviceidRes{status=%d}", res.Status)
}
