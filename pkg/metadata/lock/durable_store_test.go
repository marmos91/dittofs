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
