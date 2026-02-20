// Package types - TEST_STATEID operation types (RFC 8881 Section 18.48).
//
// TEST_STATEID tests a set of stateids for validity. The server returns
// per-stateid status codes indicating whether each stateid is valid.
package types

import (
	"bytes"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/protocol/xdr"
)

// ============================================================================
// TEST_STATEID Args (RFC 8881 Section 18.48.1)
// ============================================================================

// TestStateidArgs represents TEST_STATEID4args per RFC 8881 Section 18.48.
//
//	struct TEST_STATEID4args {
//	    stateid4 ts_stateids<>;
//	};
type TestStateidArgs struct {
	Stateids []Stateid4
}

// Encode writes the TEST_STATEID args in XDR format.
func (a *TestStateidArgs) Encode(buf *bytes.Buffer) error {
	count := uint32(len(a.Stateids))
	if err := xdr.WriteUint32(buf, count); err != nil {
		return fmt.Errorf("encode test_stateid count: %w", err)
	}
	for i := range a.Stateids {
		EncodeStateid4(buf, &a.Stateids[i])
	}
	return nil
}

// Decode reads the TEST_STATEID args from XDR format.
func (a *TestStateidArgs) Decode(r io.Reader) error {
	count, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode test_stateid count: %w", err)
	}
	if count > 1024 {
		return fmt.Errorf("test_stateid count %d exceeds limit", count)
	}
	a.Stateids = make([]Stateid4, count)
	for i := uint32(0); i < count; i++ {
		sid, err := DecodeStateid4(r)
		if err != nil {
			return fmt.Errorf("decode test_stateid[%d]: %w", i, err)
		}
		a.Stateids[i] = *sid
	}
	return nil
}

// String returns a human-readable representation.
func (a *TestStateidArgs) String() string {
	return fmt.Sprintf("TestStateidArgs{stateids=%d}", len(a.Stateids))
}

// ============================================================================
// TEST_STATEID Res (RFC 8881 Section 18.48.2)
// ============================================================================

// TestStateidRes represents TEST_STATEID4res per RFC 8881 Section 18.48.
//
//	struct TEST_STATEID4resok {
//	    nfsstat4 tsr_status_codes<>;
//	};
//	union TEST_STATEID4res switch (nfsstat4 tsr_status) {
//	    case NFS4_OK:
//	        TEST_STATEID4resok tsr_resok4;
//	    default:
//	        void;
//	};
type TestStateidRes struct {
	Status      uint32
	StatusCodes []uint32 // per-stateid status codes (only if NFS4_OK)
}

// Encode writes the TEST_STATEID result in XDR format.
func (res *TestStateidRes) Encode(buf *bytes.Buffer) error {
	if err := xdr.WriteUint32(buf, res.Status); err != nil {
		return fmt.Errorf("encode test_stateid status: %w", err)
	}
	if res.Status == NFS4_OK {
		count := uint32(len(res.StatusCodes))
		if err := xdr.WriteUint32(buf, count); err != nil {
			return fmt.Errorf("encode test_stateid status_codes count: %w", err)
		}
		for i, code := range res.StatusCodes {
			if err := xdr.WriteUint32(buf, code); err != nil {
				return fmt.Errorf("encode test_stateid status_code[%d]: %w", i, err)
			}
		}
	}
	return nil
}

// Decode reads the TEST_STATEID result from XDR format.
func (res *TestStateidRes) Decode(r io.Reader) error {
	status, err := xdr.DecodeUint32(r)
	if err != nil {
		return fmt.Errorf("decode test_stateid status: %w", err)
	}
	res.Status = status
	if res.Status == NFS4_OK {
		count, err := xdr.DecodeUint32(r)
		if err != nil {
			return fmt.Errorf("decode test_stateid status_codes count: %w", err)
		}
		if count > 1024 {
			return fmt.Errorf("test_stateid status_codes count %d exceeds limit", count)
		}
		res.StatusCodes = make([]uint32, count)
		for i := uint32(0); i < count; i++ {
			code, err := xdr.DecodeUint32(r)
			if err != nil {
				return fmt.Errorf("decode test_stateid status_code[%d]: %w", i, err)
			}
			res.StatusCodes[i] = code
		}
	}
	return nil
}

// String returns a human-readable representation.
func (res *TestStateidRes) String() string {
	if res.Status == NFS4_OK {
		return fmt.Sprintf("TestStateidRes{status=OK, codes=%v}", res.StatusCodes)
	}
	return fmt.Sprintf("TestStateidRes{status=%d}", res.Status)
}
