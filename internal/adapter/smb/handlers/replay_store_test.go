package handlers

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/pending"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// TestStoreCreateReplay_OnlySuccessIsCached pins the rule that moved out of the
// cache when the cache stopped knowing the SMB wire types. A failed CREATE must
// run the normal handler path on replay, where it may legitimately succeed;
// caching it would pin the error for the whole replay window.
func TestStoreCreateReplay_OnlySuccessIsCached(t *testing.T) {
	guid := [16]byte{1, 2, 3}

	for _, tc := range []struct {
		name   string
		status types.Status
		cached bool
	}{
		{"success is replayable", types.StatusSuccess, true},
		{"access denied is not", types.StatusAccessDenied, false},
		{"sharing violation is not", types.StatusSharingViolation, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{CreateReplayCache: pending.NewCreateReplayCache[*CreateResponse, *OpenFile]()}
			resp := &CreateResponse{SMBResponseBase: SMBResponseBase{Status: tc.status}}

			h.storeCreateReplay(1, guid, resp, nil)

			if got := h.CreateReplayCache.Len() == 1; got != tc.cached {
				t.Errorf("cached = %v, want %v for status %v", got, tc.cached, tc.status)
			}
		})
	}
}

// TestStoreCreateReplay_NilCacheAndResponse covers the guards that let callers
// invoke this unconditionally.
func TestStoreCreateReplay_NilCacheAndResponse(t *testing.T) {
	guid := [16]byte{9}

	(&Handler{}).storeCreateReplay(1, guid, &CreateResponse{}, nil) // nil cache must not panic

	h := &Handler{CreateReplayCache: pending.NewCreateReplayCache[*CreateResponse, *OpenFile]()}
	h.storeCreateReplay(1, guid, nil, nil)
	if h.CreateReplayCache.Len() != 0 {
		t.Error("a nil response must not be cached")
	}
}
