package xdr

import (
	"bytes"
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/nfs/nsm/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
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
