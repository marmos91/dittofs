// Package types - GETDEVICEINFO operation types (RFC 8881 Section 18.40).
//
// GETDEVICEINFO returns the mapping of a device ID to its storage device
// address. Used in pNFS to resolve data server locations.
// Included for wire compatibility; pNFS is out of scope for v3.0.
package types

import (
	"bytes"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/protocol/xdr"
)

// DeviceId4Size is the size of a device ID in bytes.
// Per RFC 8881 Section 3.3.14: typedef opaque deviceid4[NFS4_DEVICEID4_SIZE].
const DeviceId4Size = 16

// DeviceId4 is a fixed 16-byte device identifier.
// Encoded as fixed-size XDR opaque (no length prefix).
type DeviceId4 [DeviceId4Size]byte

// ============================================================================
// GETDEVICEINFO Args (RFC 8881 Section 18.40.1)
// ============================================================================

// GetDeviceInfoArgs represents GETDEVICEINFO4args per RFC 8881 Section 18.40.
//
//	struct GETDEVICEINFO4args {
//	    deviceid4    gdia_device_id;
//	    layouttype4  gdia_layout_type;
//	    count4       gdia_maxcount;
//	    bitmap4      gdia_notify_types;
//	};
type GetDeviceInfoArgs struct {
	DeviceID    DeviceId4
	LayoutType  uint32 // LAYOUT4_* constant
	MaxCount    uint32
	NotifyTypes Bitmap4
}

// Encode writes the GETDEVICEINFO args in XDR format.
func (a *GetDeviceInfoArgs) Encode(buf *bytes.Buffer) error {
	// Fixed 16-byte opaque, no length prefix
	if _, err := buf.Write(a.DeviceID[:]); err != nil {
		return fmt.Errorf("encode getdeviceinfo device_id: %w", err)
	}
	if err := xdr.WriteUint32(buf, a.LayoutType); err != nil {
		return fmt.Errorf("encode getdeviceinfo layout_type: %w", err)
	}
	if err := xdr.WriteUint32(buf, a.MaxCount); err != nil {
		return fmt.Errorf("encode getdeviceinfo max_count: %w", err)
	}
	if err := a.NotifyTypes.Encode(buf); err != nil {
		return fmt.Errorf("encode getdeviceinfo notify_types: %w", err)
	}
	return nil
}

// Decode reads the GETDEVICEINFO args from XDR format.
func (a *GetDeviceInfoArgs) Decode(r io.Reader) error {
	// Fixed 16-byte opaque
	if _, err := io.ReadFull(r, a.DeviceID[:]); err != nil {
		return fmt.Errorf("decode getdeviceinfo device_id: %w", err)
	}
	var err error
	if a.LayoutType, err = xdr.DecodeUint32(r); err != nil {
		return fmt.Errorf("decode getdeviceinfo layout_type: %w", err)
	}
	if a.MaxCount, err = xdr.DecodeUint32(r); err != nil {
		return fmt.Errorf("decode getdeviceinfo max_count: %w", err)
	}
	if err := a.NotifyTypes.Decode(r); err != nil {
		return fmt.Errorf("decode getdeviceinfo notify_types: %w", err)
	}
	return nil
}

// String returns a human-readable representation.
func (a *GetDeviceInfoArgs) String() string {
	return fmt.Sprintf("GetDeviceInfoArgs{device=%x, type=%d, max=%d}",
		a.DeviceID, a.LayoutType, a.MaxCount)
}

// ============================================================================
// GETDEVICEINFO Res (RFC 8881 Section 18.40.2)
// ============================================================================

// GetDeviceInfoRes represents GETDEVICEINFO4res per RFC 8881 Section 18.40.
//
//	struct device_addr4 {
//	    layouttype4 da_layout_type;
//	    opaque      da_addr_body<>;
//	};
//	struct GETDEVICEINFO4resok {
//	    device_addr4 gdir_device_addr;
//	    bitmap4      gdir_notification;
//	};
//	union GETDEVICEINFO4res switch (nfsstat4 gdir_status) {
//	    case NFS4_OK:
//	        GETDEVICEINFO4resok gdir_resok4;
//	    case NFS4ERR_TOOSMALL:
//	        count4 gdir_mincount;
//	    default:
//	        void;
//	};
type GetDeviceInfoRes struct {
	Status       uint32
	DeviceAddr   []byte  // opaque device_addr4 body (only if NFS4_OK)
	LayoutType   uint32  // layout type of device_addr4 (only if NFS4_OK)
	Notification Bitmap4 // only if NFS4_OK
	MinCount     uint32  // only if NFS4ERR_TOOSMALL
}

// Encode writes the GETDEVICEINFO result in XDR format.
func (res *GetDeviceInfoRes) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint32(buf, res.Status); err != nil {
		return fmt.Errorf("encode getdeviceinfo status: %w", err)
	}
	switch res.Status {
	case NFS4_OK:
		// device_addr4: layout_type + opaque body
		if err := xdr.WriteUint32(buf, res.LayoutType); err != nil {
			return fmt.Errorf("encode getdeviceinfo device_addr layout_type: %w", err)
		}
		if err := xdr.WriteXDROpaque(buf, res.DeviceAddr); err != nil {
			return fmt.Errorf("encode getdeviceinfo device_addr body: %w", err)
		}
		if err := res.Notification.Encode(buf); err != nil {
			return fmt.Errorf("encode getdeviceinfo notification: %w", err)
		}
	case NFS4ERR_TOOSMALL:
		if err := xdr.WriteUint32(buf, res.MinCount); err != nil {
			return fmt.Errorf("encode getdeviceinfo min_count: %w", err)
		}
	default:
		// void
	}
	return nil
}

// Decode reads the GETDEVICEINFO result from XDR format.
func (res *GetDeviceInfoRes) Decode(r io.Reader) error {
	status, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode getdeviceinfo status: %w", err)
	}
	res.Status = status
	switch res.Status {
	case NFS4_OK:
		if res.LayoutType, err = xdr.DecodeUint32(r); err != nil {
			return fmt.Errorf("decode getdeviceinfo device_addr layout_type: %w", err)
		}
		if res.DeviceAddr, err = xdr.DecodeOpaque(r); err != nil {
			return fmt.Errorf("decode getdeviceinfo device_addr body: %w", err)
		}
		if err := res.Notification.Decode(r); err != nil {
			return fmt.Errorf("decode getdeviceinfo notification: %w", err)
		}
	case NFS4ERR_TOOSMALL:
		if res.MinCount, err = xdr.DecodeUint32(r); err != nil {
			return fmt.Errorf("decode getdeviceinfo min_count: %w", err)
		}
	default:
		// void
	}
	return nil
}

// String returns a human-readable representation.
func (res *GetDeviceInfoRes) String() string {
	if res.Status == NFS4_OK {
		return fmt.Sprintf("GetDeviceInfoRes{status=OK, type=%d, addr=%d bytes}",
			res.LayoutType, len(res.DeviceAddr))
	}
	return fmt.Sprintf("GetDeviceInfoRes{status=%d}", res.Status)
}
