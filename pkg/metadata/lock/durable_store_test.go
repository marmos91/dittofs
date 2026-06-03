package lock

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateReconnect_Matches(t *testing.T) {
	h := &PersistedDurableHandle{
		ShareName: "share",
		Username:  "alice",
		Path:      "dir/file.txt",
	}

	// Path check off: filename is ignored (oplock-backed reconnect).
	err := h.ValidateReconnect(ReconnectIdentity{
		ShareName: "share",
		Username:  "alice",
		Filename:  "junk-name",
		CheckPath: false,
	})
	assert.NoError(t, err)

	// Path check on: filename must match the persisted path.
	err = h.ValidateReconnect(ReconnectIdentity{
		ShareName: "share",
		Username:  "alice",
		Filename:  "dir/file.txt",
		CheckPath: true,
	})
	assert.NoError(t, err)
}

func TestValidateReconnect_Mismatch(t *testing.T) {
	h := &PersistedDurableHandle{
		ShareName: "share",
		Username:  "alice",
		Path:      "dir/file.txt",
	}

	cases := []struct {
		name string
		req  ReconnectIdentity
		kind DurableMismatchKind
	}{
		{
			name: "share",
			req:  ReconnectIdentity{ShareName: "other", Username: "alice"},
			kind: MismatchShare,
		},
		{
			name: "path",
			req:  ReconnectIdentity{ShareName: "share", Username: "alice", Filename: "wrong", CheckPath: true},
			kind: MismatchPath,
		},
		{
			name: "user",
			req:  ReconnectIdentity{ShareName: "share", Username: "bob"},
			kind: MismatchUser,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := h.ValidateReconnect(tc.req)
			require.Error(t, err)
			var mismatch *DurableHandleMismatchError
			require.True(t, errors.As(err, &mismatch))
			assert.Equal(t, tc.kind, mismatch.Kind)
		})
	}
}

// Share mismatch takes precedence over a path mismatch, mirroring the order the
// reconnect ladder checks fields in.
func TestValidateReconnect_ShareBeforePath(t *testing.T) {
	h := &PersistedDurableHandle{ShareName: "share", Username: "alice", Path: "p"}
	err := h.ValidateReconnect(ReconnectIdentity{
		ShareName: "other",
		Username:  "alice",
		Filename:  "q",
		CheckPath: true,
	})
	var mismatch *DurableHandleMismatchError
	require.ErrorAs(t, err, &mismatch)
	assert.Equal(t, MismatchShare, mismatch.Kind)
}

// LockOpenID derives the byte-range-lock OpenID from OriginalFileID when set,
// falling back to FileID for legacy rows persisted before OriginalFileID
// existed. The cleanup paths (scavenger, AppInstanceId failover, ForceClose)
// rely on this to release locks recorded under hex(OriginalFileID).
func TestLockOpenID(t *testing.T) {
	t.Run("uses OriginalFileID when set", func(t *testing.T) {
		h := &PersistedDurableHandle{
			FileID:         [16]byte{0xAA},
			OriginalFileID: [16]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08},
		}
		// Full 16-byte hex (32 chars), derived from OriginalFileID not FileID.
		assert.Equal(t, "01020304050607080000000000000000", h.LockOpenID())
	})

	t.Run("falls back to FileID when OriginalFileID is zero", func(t *testing.T) {
		h := &PersistedDurableHandle{
			FileID: [16]byte{0xDE, 0xAD, 0xBE, 0xEF},
		}
		assert.Equal(t, "deadbeef000000000000000000000000", h.LockOpenID())
	})

	t.Run("OriginalFileID takes precedence even when FileID differs", func(t *testing.T) {
		orig := [16]byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88}
		h := &PersistedDurableHandle{
			FileID:         [16]byte{0x99},
			OriginalFileID: orig,
		}
		assert.Equal(t, "11223344556677880000000000000000", h.LockOpenID())
	})
}
