package xdr

import (
	"bytes"
	"fmt"

	"github.com/marmos91/dittofs/internal/protocol/nsm/types"
	"github.com/marmos91/dittofs/internal/protocol/xdr"
)

// ============================================================================
// NSM Response Encoding
// ============================================================================

// EncodeSMStatRes encodes SM_STAT/SM_MON response (sm_stat_res structure).
//
// XDR format:
//
//	struct sm_stat_res {
//	    sm_res   res_stat;
//	    int      state;
//	};
func EncodeSMStatRes(res *types.SMStatRes) ([]byte, error) {
	buf := new(bytes.Buffer)

	if err := xdr.WriteUint32(buf, res.Result); err != nil {
		return nil, fmt.Errorf("encode result: %w", err)
	}

	if err := xdr.WriteInt32(buf, res.State); err != nil {
		return nil, fmt.Errorf("encode state: %w", err)
	}

	return buf.Bytes(), nil
}

// EncodeSMStat encodes SM_STAT-only response (sm_stat structure).
//
// XDR format:
//
//	struct sm_stat {
//	    int state;
//	};
func EncodeSMStat(stat *types.SMStat) ([]byte, error) {
	buf := new(bytes.Buffer)

	if err := xdr.WriteInt32(buf, stat.State); err != nil {
		return nil, fmt.Errorf("encode state: %w", err)
	}

	return buf.Bytes(), nil
}

// EncodeStatus encodes SM_NOTIFY callback payload (status structure).
//
// XDR format:
//
//	struct status {
//	    string   mon_name<SM_MAXSTRLEN>;
//	    int      state;
//	    opaque   priv[16];
//	};
//
// Note: priv is a fixed-size opaque[16], not variable-length.
// Per XDR, fixed-size opaque has no length prefix.
func EncodeStatus(status *types.Status) ([]byte, error) {
	buf := new(bytes.Buffer)

	if err := xdr.WriteXDRString(buf, status.MonName); err != nil {
		return nil, fmt.Errorf("encode mon_name: %w", err)
	}

	if err := xdr.WriteInt32(buf, status.State); err != nil {
		return nil, fmt.Errorf("encode state: %w", err)
	}

	// Encode priv as opaque[16] (fixed size, no length prefix)
	if _, err := buf.Write(status.Priv[:]); err != nil {
		return nil, fmt.Errorf("encode priv: %w", err)
	}

	return buf.Bytes(), nil
}

// EncodeSmName encodes an sm_name structure (for SM_UNMON response).
//
// XDR format:
//
//	struct sm_name {
//	    string mon_name<SM_MAXSTRLEN>;
//	};
func EncodeSmName(name *types.SMName) ([]byte, error) {
	buf := new(bytes.Buffer)

	if err := xdr.WriteXDRString(buf, name.Name); err != nil {
		return nil, fmt.Errorf("encode name: %w", err)
	}

	return buf.Bytes(), nil
}
