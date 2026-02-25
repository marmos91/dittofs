// Package types - GETDEVICELIST operation types (RFC 8881 Section 18.41).
//
// GETDEVICELIST returns a list of device IDs for a given layout type.
// Included for wire compatibility; pNFS is out of scope for v3.0.
package types

import (
	"bytes"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
)

// ============================================================================
// GETDEVICELIST Args (RFC 8881 Section 18.41.1)
// ============================================================================

// GetDeviceListArgs represents GETDEVICELIST4args per RFC 8881 Section 18.41.
//
//	struct GETDEVICELIST4args {
//	    layouttype4  gdla_layout_type;
//	    count4       gdla_maxdevices;
//	    nfs_cookie4  gdla_cookie;
//	    verifier4    gdla_cookieverf;
//	};
type GetDeviceListArgs struct {
	LayoutType uint32
	MaxDevices uint32
	Cookie     uint64
	CookieVerf [8]byte
}

// Encode writes the GETDEVICELIST args in XDR format.
func (a *GetDeviceListArgs) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint32(buf, a.LayoutType); err != nil {
		return fmt.Errorf("encode getdevicelist layout_type: %w", err)
	}
	if err := xdr.WriteUint32(buf, a.MaxDevices); err != nil {
		return fmt.Errorf("encode getdevicelist max_devices: %w", err)
	}
	if err := xdr.WriteUint64(buf, a.Cookie); err != nil {
		return fmt.Errorf("encode getdevicelist cookie: %w", err)
	}
	if _, err := buf.Write(a.CookieVerf[:]); err != nil {
		return fmt.Errorf("encode getdevicelist cookie_verf: %w", err)
	}
	return nil
}

// Decode reads the GETDEVICELIST args from XDR format.
func (a *GetDeviceListArgs) Decode(r io.Reader) error {
	var err error
	if a.LayoutType, err = xdr.DecodeUint32(r); err != nil {
		return fmt.Errorf("decode getdevicelist layout_type: %w", err)
	}
	if a.MaxDevices, err = xdr.DecodeUint32(r); err != nil {
		return fmt.Errorf("decode getdevicelist max_devices: %w", err)
	}
	if a.Cookie, err = xdr.DecodeUint64(r); err != nil {
		return fmt.Errorf("decode getdevicelist cookie: %w", err)
	}
	if _, err := io.ReadFull(r, a.CookieVerf[:]); err != nil {
		return fmt.Errorf("decode getdevicelist cookie_verf: %w", err)
	}
	return nil
}

// String returns a human-readable representation.
func (a *GetDeviceListArgs) String() string {
	return fmt.Sprintf("GetDeviceListArgs{type=%d, max=%d, cookie=%d}",
		a.LayoutType, a.MaxDevices, a.Cookie)
}

// ============================================================================
// GETDEVICELIST Res (RFC 8881 Section 18.41.2)
// ============================================================================

// GetDeviceListRes represents GETDEVICELIST4res per RFC 8881 Section 18.41.
//
//	struct GETDEVICELIST4resok {
//	    nfs_cookie4  gdlr_cookie;
//	    verifier4    gdlr_cookieverf;
//	    deviceid4    gdlr_deviceid_list<>;
//	    bool         gdlr_eof;
//	};
//	union GETDEVICELIST4res switch (nfsstat4 gdlr_status) {
//	    case NFS4_OK:
//	        GETDEVICELIST4resok gdlr_resok4;
//	    default:
//	        void;
//	};
type GetDeviceListRes struct {
	Status     uint32
	Cookie     uint64      // only if NFS4_OK
	CookieVerf [8]byte     // only if NFS4_OK
	DeviceIDs  []DeviceId4 // only if NFS4_OK
	EOF        bool        // only if NFS4_OK
}

// Encode writes the GETDEVICELIST result in XDR format.
func (res *GetDeviceListRes) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint32(buf, res.Status); err != nil {
		return fmt.Errorf("encode getdevicelist status: %w", err)
	}
	if res.Status == NFS4_OK {
		if err := xdr.WriteUint64(buf, res.Cookie); err != nil {
			return fmt.Errorf("encode getdevicelist cookie: %w", err)
		}
		if _, err := buf.Write(res.CookieVerf[:]); err != nil {
			return fmt.Errorf("encode getdevicelist cookie_verf: %w", err)
		}
		count := uint32(len(res.DeviceIDs))
		if err := xdr.WriteUint32(buf, count); err != nil {
			return fmt.Errorf("encode getdevicelist device_ids count: %w", err)
		}
		for i := range res.DeviceIDs {
			if _, err := buf.Write(res.DeviceIDs[i][:]); err != nil {
				return fmt.Errorf("encode getdevicelist device_id[%d]: %w", i, err)
			}
		}
		if err := xdr.WriteBool(buf, res.EOF); err != nil {
			return fmt.Errorf("encode getdevicelist eof: %w", err)
		}
	}
	return nil
}

// Decode reads the GETDEVICELIST result from XDR format.
func (res *GetDeviceListRes) Decode(r io.Reader) error {
	status, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode getdevicelist status: %w", err)
	}
	res.Status = status
	if res.Status == NFS4_OK {
		if res.Cookie, err = xdr.DecodeUint64(r); err != nil {
			return fmt.Errorf("decode getdevicelist cookie: %w", err)
		}
		if _, err := io.ReadFull(r, res.CookieVerf[:]); err != nil {
			return fmt.Errorf("decode getdevicelist cookie_verf: %w", err)
		}
		count, err := xdr.DecodeUint32(r)
		if err != nil {
			return fmt.Errorf("decode getdevicelist device_ids count: %w", err)
		}
		if count > 1024 {
			return fmt.Errorf("getdevicelist device_ids count %d exceeds limit", count)
		}
		res.DeviceIDs = make([]DeviceId4, count)
		for i := uint32(0); i < count; i++ {
			if _, err := io.ReadFull(r, res.DeviceIDs[i][:]); err != nil {
				return fmt.Errorf("decode getdevicelist device_id[%d]: %w", i, err)
			}
		}
		eof, err := xdr.DecodeBool(r)
		if err != nil {
			return fmt.Errorf("decode getdevicelist eof: %w", err)
		}
		res.EOF = eof
	}
	return nil
}

// String returns a human-readable representation.
func (res *GetDeviceListRes) String() string {
	if res.Status == NFS4_OK {
		return fmt.Sprintf("GetDeviceListRes{status=OK, devices=%d, eof=%t}",
			len(res.DeviceIDs), res.EOF)
	}
	return fmt.Sprintf("GetDeviceListRes{status=%d}", res.Status)
}
