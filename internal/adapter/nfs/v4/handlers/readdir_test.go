package handlers

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/attrs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// TestReadDir_ArgumentValidation covers the READDIR arguments RFC 7530
// Section 16.24.4 requires the server to refuse: the cookie values reserved
// for the client's own "." and "..", a maxcount too small to hold even an
// empty listing, and an attribute request naming a settable-only attribute
// the encoder has no value to write for.
func TestReadDir_ArgumentValidation(t *testing.T) {
	tests := []struct {
		name     string
		cookie   uint64
		maxcount uint32
		bits     []uint32
		want     uint32
	}{
		{"reserved cookie 1", 1, 8192, []uint32{attrs.FATTR4_TYPE}, types.NFS4ERR_BAD_COOKIE},
		{"reserved cookie 2", 2, 8192, []uint32{attrs.FATTR4_TYPE}, types.NFS4ERR_BAD_COOKIE},
		{"maxcount zero", 0, 0, []uint32{attrs.FATTR4_TYPE}, types.NFS4ERR_TOOSMALL},
		// Cookie 4 is past the last child, so the listing is empty and only
		// the up-front maxcount check can reject it.
		{"maxcount below an empty listing", 4, 15, []uint32{attrs.FATTR4_TYPE}, types.NFS4ERR_TOOSMALL},
		{"write-only time_access_set", 0, 8192, []uint32{attrs.FATTR4_TIME_ACCESS_SET}, types.NFS4ERR_INVAL},
		{"write-only time_modify_set", 0, 8192, []uint32{attrs.FATTR4_TIME_MODIFY_SET}, types.NFS4ERR_INVAL},
		{"accepted", 0, 8192, []uint32{attrs.FATTR4_TYPE}, types.NFS4_OK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTestHandlerWithShares([]string{"/export", "/data/archive"})
			ctx := newOpsTestContext()

			data := encodeCompoundWithOps("", 0, []encodedOp{
				encodePutRootFH(),
				encodeReadDir(tt.cookie, tt.maxcount, tt.bits...),
			})

			resp, err := h.ProcessCompound(ctx, data)
			if err != nil {
				t.Fatalf("ProcessCompound error: %v", err)
			}

			decoded, _ := decodeCompoundResp(resp)
			if decoded.Results[1].Status != tt.want {
				t.Errorf("READDIR status = %d, want %d", decoded.Results[1].Status, tt.want)
			}
		})
	}
}
