package handlers

import (
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

func TestCreateReplayCache_StoreLookup(t *testing.T) {
	c := NewCreateReplayCache()
	guid := [16]byte{1, 2, 3, 4}
	resp := &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess}}
	c.Store(42, guid, resp)
	if got := c.Lookup(42, guid); got != resp {
		t.Fatalf("Lookup returned %v, want %v", got, resp)
	}
}

func TestCreateReplayCache_LookupWrongSession(t *testing.T) {
	c := NewCreateReplayCache()
	guid := [16]byte{1}
	c.Store(1, guid, &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess}})
	if c.Lookup(2, guid) != nil {
		t.Fatal("Lookup across sessions must miss")
	}
}

func TestCreateReplayCache_ZeroGuidIgnored(t *testing.T) {
	c := NewCreateReplayCache()
	zero := [16]byte{}
	c.Store(1, zero, &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess}})
	if c.Len() != 0 {
		t.Fatal("zero CreateGuid must not be stored")
	}
	if c.Lookup(1, zero) != nil {
		t.Fatal("zero CreateGuid must not be looked up")
	}
}

func TestCreateReplayCache_NonSuccessNotCached(t *testing.T) {
	c := NewCreateReplayCache()
	guid := [16]byte{7}
	c.Store(1, guid, &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusAccessDenied}})
	if c.Len() != 0 {
		t.Fatal("non-success response must not be cached")
	}
}

func TestCreateReplayCache_TTLExpiry(t *testing.T) {
	c := NewCreateReplayCache()
	guid := [16]byte{9}
	c.Store(1, guid, &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess}})
	// Force expiry by rewriting StoredAt
	c.mu.Lock()
	e := c.entries[createReplayKey{CreateGuid: guid}]
	e.StoredAt = time.Now().Add(-2 * replayCacheTTL)
	c.entries[createReplayKey{CreateGuid: guid}] = e
	c.mu.Unlock()
	if c.Lookup(1, guid) != nil {
		t.Fatal("expired entry must miss")
	}
}

func TestCreateReplayCache_Forget(t *testing.T) {
	c := NewCreateReplayCache()
	guid := [16]byte{3}
	c.Store(1, guid, &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess}})
	c.Forget(guid)
	if c.Len() != 0 {
		t.Fatal("Forget must drop entry")
	}
}

func TestCreateReplayCache_ForgetSession(t *testing.T) {
	c := NewCreateReplayCache()
	g1, g2 := [16]byte{1}, [16]byte{2}
	c.Store(10, g1, &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess}})
	c.Store(20, g2, &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess}})
	c.ForgetSession(10)
	if c.Lookup(10, g1) != nil {
		t.Fatal("ForgetSession should drop session 10 entry")
	}
	if c.Lookup(20, g2) == nil {
		t.Fatal("ForgetSession should leave other session entries")
	}
}

func TestUnpackLockSequence(t *testing.T) {
	cases := []struct {
		packed     uint32
		wantIndex  uint8
		wantNumber uint32
		wantOK     bool
	}{
		{0, 0, 0, false},
		{0x1000_0001, 1, 1, true},
		{0xF000_0000, 15, 0, true},
		{0xF0FF_FFFF, 15, 0x00FF_FFFF, true},
		{0x0FFF_FFFF, 0, 0x0FFF_FFFF, true},
	}
	for _, tc := range cases {
		idx, num, ok := UnpackLockSequence(tc.packed)
		if idx != tc.wantIndex || num != tc.wantNumber || ok != tc.wantOK {
			t.Errorf("UnpackLockSequence(%#x) = (%d, %d, %v), want (%d, %d, %v)",
				tc.packed, idx, num, ok, tc.wantIndex, tc.wantNumber, tc.wantOK)
		}
	}
}

func TestLockReplayCache_StoreLookup(t *testing.T) {
	c := NewLockReplayCache()
	fid := [16]byte{0xAA}
	c.Store(fid, 3, 7, types.StatusSuccess)
	if st, ok := c.Lookup(fid, 3, 7); !ok || st != types.StatusSuccess {
		t.Fatalf("Lookup = (%v, %v), want (StatusSuccess, true)", st, ok)
	}
}

func TestLockReplayCache_MissDifferentNumber(t *testing.T) {
	c := NewLockReplayCache()
	fid := [16]byte{0xBB}
	c.Store(fid, 1, 5, types.StatusSuccess)
	if _, ok := c.Lookup(fid, 1, 6); ok {
		t.Fatal("Lookup with different Number must miss")
	}
}

func TestLockReplayCache_IndexBoundsRejected(t *testing.T) {
	c := NewLockReplayCache()
	fid := [16]byte{0xCC}
	c.Store(fid, LockSequenceIndexMax, 1, types.StatusSuccess)
	if c.Len() != 0 {
		t.Fatal("Store with out-of-range index must be ignored")
	}
	if _, ok := c.Lookup(fid, LockSequenceIndexMax, 1); ok {
		t.Fatal("Lookup with out-of-range index must miss")
	}
}

func TestLockReplayCache_ForgetFile(t *testing.T) {
	c := NewLockReplayCache()
	fid1 := [16]byte{1}
	fid2 := [16]byte{2}
	c.Store(fid1, 0, 1, types.StatusSuccess)
	c.Store(fid1, 5, 9, types.StatusSuccess)
	c.Store(fid2, 0, 1, types.StatusSuccess)
	c.ForgetFile(fid1)
	if _, ok := c.Lookup(fid1, 0, 1); ok {
		t.Fatal("ForgetFile must drop entries for fid1 slot 0")
	}
	if _, ok := c.Lookup(fid1, 5, 9); ok {
		t.Fatal("ForgetFile must drop entries for fid1 slot 5")
	}
	if _, ok := c.Lookup(fid2, 0, 1); !ok {
		t.Fatal("ForgetFile must not touch fid2")
	}
}

func TestLockReplayCache_OverwriteUpdatesNumber(t *testing.T) {
	c := NewLockReplayCache()
	fid := [16]byte{0xDD}
	c.Store(fid, 2, 10, types.StatusSuccess)
	c.Store(fid, 2, 11, types.StatusLockNotGranted)
	if _, ok := c.Lookup(fid, 2, 10); ok {
		t.Fatal("Lookup with previous Number must miss after overwrite")
	}
	st, ok := c.Lookup(fid, 2, 11)
	if !ok || st != types.StatusLockNotGranted {
		t.Fatalf("Lookup = (%v, %v), want (StatusLockNotGranted, true)", st, ok)
	}
}
