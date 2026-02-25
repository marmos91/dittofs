// Package types - GET_DIR_DELEGATION operation types (RFC 8881 Section 18.39).
//
// GET_DIR_DELEGATION allows a client to request directory change notifications
// from the server. The server can grant or deny the delegation.
package types

import (
	"bytes"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
)

// GET_DIR_DELEGATION response status constants.
const (
	GDD4_OK      = 0 // Delegation granted
	GDD4_UNAVAIL = 1 // Delegation unavailable
)

// ============================================================================
// GET_DIR_DELEGATION Args (RFC 8881 Section 18.39.1)
// ============================================================================

// GetDirDelegationArgs represents GET_DIR_DELEGATION4args per RFC 8881 Section 18.39.
//
//	struct GET_DIR_DELEGATION4args {
//	    bool     gdda_signal_deleg_avail;
//	    bitmap4  gdda_notification_types;
//	    attr_notice4 gdda_child_attr_delay;
//	    attr_notice4 gdda_dir_attr_delay;
//	    bitmap4  gdda_child_attributes;
//	    bitmap4  gdda_dir_attributes;
//	};
type GetDirDelegationArgs struct {
	Signal            bool     // whether client signals delegation availability
	NotificationTypes Bitmap4  // which changes to notify about
	ChildAttrDelay    NFS4Time // delay before child attr notifications
	DirAttrDelay      NFS4Time // delay before dir attr notifications
	ChildAttrs        Bitmap4  // child attributes of interest
	DirAttrs          Bitmap4  // directory attributes of interest
}

// Encode writes the GET_DIR_DELEGATION args in XDR format.
func (a *GetDirDelegationArgs) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteBool(buf, a.Signal); err != nil {
		return fmt.Errorf("encode gdd signal: %w", err)
	}
	if err := a.NotificationTypes.Encode(buf); err != nil {
		return fmt.Errorf("encode gdd notification_types: %w", err)
	}
	// attr_notice4 is nfstime4
	if err := xdr.WriteInt64(buf, a.ChildAttrDelay.Seconds); err != nil {
		return fmt.Errorf("encode gdd child_attr_delay.seconds: %w", err)
	}
	if err := xdr.WriteUint32(buf, a.ChildAttrDelay.Nseconds); err != nil {
		return fmt.Errorf("encode gdd child_attr_delay.nseconds: %w", err)
	}
	if err := xdr.WriteInt64(buf, a.DirAttrDelay.Seconds); err != nil {
		return fmt.Errorf("encode gdd dir_attr_delay.seconds: %w", err)
	}
	if err := xdr.WriteUint32(buf, a.DirAttrDelay.Nseconds); err != nil {
		return fmt.Errorf("encode gdd dir_attr_delay.nseconds: %w", err)
	}
	if err := a.ChildAttrs.Encode(buf); err != nil {
		return fmt.Errorf("encode gdd child_attrs: %w", err)
	}
	if err := a.DirAttrs.Encode(buf); err != nil {
		return fmt.Errorf("encode gdd dir_attrs: %w", err)
	}
	return nil
}

// Decode reads the GET_DIR_DELEGATION args from XDR format.
func (a *GetDirDelegationArgs) Decode(r io.Reader) error {
	signal, err := xdr.DecodeBool(r)
	if err != nil {
		return fmt.Errorf("decode gdd signal: %w", err)
	}
	a.Signal = signal
	if err := a.NotificationTypes.Decode(r); err != nil {
		return fmt.Errorf("decode gdd notification_types: %w", err)
	}
	sec, err := xdr.DecodeInt64(r)
	if err != nil {
		return fmt.Errorf("decode gdd child_attr_delay.seconds: %w", err)
	}
	nsec, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode gdd child_attr_delay.nseconds: %w", err)
	}
	a.ChildAttrDelay = NFS4Time{Seconds: sec, Nseconds: nsec}
	sec, err = xdr.DecodeInt64(r)
	if err != nil {
		return fmt.Errorf("decode gdd dir_attr_delay.seconds: %w", err)
	}
	nsec, err = xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode gdd dir_attr_delay.nseconds: %w", err)
	}
	a.DirAttrDelay = NFS4Time{Seconds: sec, Nseconds: nsec}
	if err := a.ChildAttrs.Decode(r); err != nil {
		return fmt.Errorf("decode gdd child_attrs: %w", err)
	}
	if err := a.DirAttrs.Decode(r); err != nil {
		return fmt.Errorf("decode gdd dir_attrs: %w", err)
	}
	return nil
}

// String returns a human-readable representation.
func (a *GetDirDelegationArgs) String() string {
	return fmt.Sprintf("GetDirDelegationArgs{signal=%t, notif=%s, child_delay=%d.%09d, dir_delay=%d.%09d}",
		a.Signal, a.NotificationTypes.String(),
		a.ChildAttrDelay.Seconds, a.ChildAttrDelay.Nseconds,
		a.DirAttrDelay.Seconds, a.DirAttrDelay.Nseconds)
}

// ============================================================================
// GET_DIR_DELEGATION Res (RFC 8881 Section 18.39.2)
// ============================================================================

// GetDirDelegationResOK holds the successful response fields for GET_DIR_DELEGATION.
type GetDirDelegationResOK struct {
	CookieVerf        [8]byte
	Stateid           Stateid4
	NotificationTypes Bitmap4
	ChildAttrs        Bitmap4
	DirAttrs          Bitmap4
}

// GetDirDelegationRes represents GET_DIR_DELEGATION4res per RFC 8881 Section 18.39.
//
//	union GET_DIR_DELEGATION4res switch (nfsstat4 gddr_status) {
//	    case NFS4_OK:
//	        GET_DIR_DELEGATION4res_non_fatal gddr_res_non_fatal4;
//	    default:
//	        void;
//	};
//
//	union GET_DIR_DELEGATION4res_non_fatal switch (gddrnf4_status status) {
//	    case GDD4_OK:
//	        GET_DIR_DELEGATION4resok gddr_res_ok4;
//	    case GDD4_UNAVAIL:
//	        bool gddr_will_signal_deleg_avail;
//	};
type GetDirDelegationRes struct {
	Status               uint32
	NonFatalStatus       uint32                 // GDD4_OK or GDD4_UNAVAIL (only if NFS4_OK)
	OK                   *GetDirDelegationResOK // only if GDD4_OK
	WillSignalDelegAvail bool                   // only if GDD4_UNAVAIL
}

// Encode writes the GET_DIR_DELEGATION result in XDR format.
func (res *GetDirDelegationRes) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint32(buf, res.Status); err != nil {
		return fmt.Errorf("encode gdd status: %w", err)
	}
	if res.Status != NFS4_OK {
		return nil
	}
	if err := xdr.WriteUint32(buf, res.NonFatalStatus); err != nil {
		return fmt.Errorf("encode gdd non_fatal_status: %w", err)
	}
	switch res.NonFatalStatus {
	case GDD4_OK:
		if res.OK == nil {
			return fmt.Errorf("gdd GDD4_OK but ok is nil")
		}
		if _, err := buf.Write(res.OK.CookieVerf[:]); err != nil {
			return fmt.Errorf("encode gdd cookie_verf: %w", err)
		}
		EncodeStateid4(buf, &res.OK.Stateid)
		if err := res.OK.NotificationTypes.Encode(buf); err != nil {
			return fmt.Errorf("encode gdd notification_types: %w", err)
		}
		if err := res.OK.ChildAttrs.Encode(buf); err != nil {
			return fmt.Errorf("encode gdd child_attrs: %w", err)
		}
		if err := res.OK.DirAttrs.Encode(buf); err != nil {
			return fmt.Errorf("encode gdd dir_attrs: %w", err)
		}
	case GDD4_UNAVAIL:
		if err := xdr.WriteBool(buf, res.WillSignalDelegAvail); err != nil {
			return fmt.Errorf("encode gdd will_signal: %w", err)
		}
	default:
		return fmt.Errorf("unknown gdd non_fatal_status: %d", res.NonFatalStatus)
	}
	return nil
}

// Decode reads the GET_DIR_DELEGATION result from XDR format.
func (res *GetDirDelegationRes) Decode(r io.Reader) error {
	status, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode gdd status: %w", err)
	}
	res.Status = status
	if res.Status != NFS4_OK {
		return nil
	}
	nfStatus, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode gdd non_fatal_status: %w", err)
	}
	res.NonFatalStatus = nfStatus
	switch res.NonFatalStatus {
	case GDD4_OK:
		ok := &GetDirDelegationResOK{}
		if _, err := io.ReadFull(r, ok.CookieVerf[:]); err != nil {
			return fmt.Errorf("decode gdd cookie_verf: %w", err)
		}
		sid, err := DecodeStateid4(r)
		if err != nil {
			return fmt.Errorf("decode gdd stateid: %w", err)
		}
		ok.Stateid = *sid
		if err := ok.NotificationTypes.Decode(r); err != nil {
			return fmt.Errorf("decode gdd notification_types: %w", err)
		}
		if err := ok.ChildAttrs.Decode(r); err != nil {
			return fmt.Errorf("decode gdd child_attrs: %w", err)
		}
		if err := ok.DirAttrs.Decode(r); err != nil {
			return fmt.Errorf("decode gdd dir_attrs: %w", err)
		}
		res.OK = ok
	case GDD4_UNAVAIL:
		willSignal, err := xdr.DecodeBool(r)
		if err != nil {
			return fmt.Errorf("decode gdd will_signal: %w", err)
		}
		res.WillSignalDelegAvail = willSignal
	default:
		return fmt.Errorf("unknown gdd non_fatal_status: %d", res.NonFatalStatus)
	}
	return nil
}

// String returns a human-readable representation.
func (res *GetDirDelegationRes) String() string {
	if res.Status != NFS4_OK {
		return fmt.Sprintf("GetDirDelegationRes{status=%d}", res.Status)
	}
	if res.NonFatalStatus == GDD4_OK && res.OK != nil {
		return fmt.Sprintf("GetDirDelegationRes{status=OK, deleg=OK, stateid={seq=%d}}",
			res.OK.Stateid.Seqid)
	}
	return fmt.Sprintf("GetDirDelegationRes{status=OK, deleg=UNAVAIL, will_signal=%t}",
		res.WillSignalDelegAvail)
}
