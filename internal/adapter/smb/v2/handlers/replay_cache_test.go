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
	c.Store(42, guid, resp, nil)
	if got := c.Lookup(42, guid); got != resp {
		t.Fatalf("Lookup returned %v, want %v", got, resp)
	}
}

func TestCreateReplayCache_LookupWrongSession(t *testing.T) {
	c := NewCreateReplayCache()
	guid := [16]byte{1}
	c.Store(1, guid, &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess}}, nil)
	if c.Lookup(2, guid) != nil {
		t.Fatal("Lookup across sessions must miss")
	}
}

func TestCreateReplayCache_ZeroGuidIgnored(t *testing.T) {
	c := NewCreateReplayCache()
	zero := [16]byte{}
	c.Store(1, zero, &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess}}, nil)
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
	c.Store(1, guid, &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusAccessDenied}}, nil)
	if c.Len() != 0 {
		t.Fatal("non-success response must not be cached")
	}
}

func TestCreateReplayCache_TTLExpiry(t *testing.T) {
	c := NewCreateReplayCache()
	guid := [16]byte{9}
	c.Store(1, guid, &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess}}, nil)
	// Force expiry by rewriting StoredAt
	c.mu.Lock()
	c.entries[guid].StoredAt = time.Now().Add(-2 * replayCacheTTL)
	c.mu.Unlock()
	if c.Lookup(1, guid) != nil {
		t.Fatal("expired entry must miss")
	}
}

func TestCreateReplayCache_Forget(t *testing.T) {
	c := NewCreateReplayCache()
	guid := [16]byte{3}
	c.Store(1, guid, &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess}}, nil)
	c.Forget(guid)
	if c.Len() != 0 {
		t.Fatal("Forget must drop entry")
	}
}

func TestCreateReplayCache_ForgetSession(t *testing.T) {
	c := NewCreateReplayCache()
	g1, g2 := [16]byte{1}, [16]byte{2}
	c.Store(10, g1, &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess}}, nil)
	c.Store(20, g2, &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess}}, nil)
	c.ForgetSession(10)
	if c.Lookup(10, g1) != nil {
		t.Fatal("ForgetSession should drop session 10 entry")
	}
	if c.Lookup(20, g2) == nil {
		t.Fatal("ForgetSession should leave other session entries")
	}
}

func TestCreateReplayCache_Reservation(t *testing.T) {
	c := NewCreateReplayCache()
	guid := [16]byte{0x5A}

	if c.IsReserved(1, guid) {
		t.Fatal("fresh cache must not report a reservation")
	}
	c.Reserve(1, guid)
	if !c.IsReserved(1, guid) {
		t.Fatal("Reserve must make IsReserved true")
	}
	// Reservation is per-session.
	if c.IsReserved(2, guid) {
		t.Fatal("reservation must be scoped to its session")
	}
	c.Release(1, guid)
	if c.IsReserved(1, guid) {
		t.Fatal("Release must clear the reservation")
	}
}

func TestCreateReplayCache_ZeroGuidReservationIgnored(t *testing.T) {
	c := NewCreateReplayCache()
	zero := [16]byte{}
	c.Reserve(1, zero)
	if c.IsReserved(1, zero) {
		t.Fatal("zero CreateGuid must never be reserved")
	}
}

func TestCreateReplayCache_ForgetSessionClearsReservations(t *testing.T) {
	c := NewCreateReplayCache()
	g1, g2 := [16]byte{1}, [16]byte{2}
	c.Reserve(10, g1)
	c.Reserve(20, g2)
	c.ForgetSession(10)
	if c.IsReserved(10, g1) {
		t.Fatal("ForgetSession must clear reservations for that session")
	}
	if !c.IsReserved(20, g2) {
		t.Fatal("ForgetSession must leave other sessions' reservations")
	}
}

func TestUnpackLockSequence(t *testing.T) {
	// Layout per MS-SMB2 §2.2.26 / Samba: low 4 bits = number,
	// upper 28 bits = bucket index. Bucket 0 → "not tracked".
	cases := []struct {
		packed     uint32
		wantIndex  uint32
		wantNumber uint8
		wantOK     bool
	}{
		{0, 0, 0, false},
		{0x0000_0001, 0, 1, false},         // bucket=0 → disabled
		{0x0000_0010, 1, 0, true},          // bucket=1, value=0
		{0x12345678, 0x1234567, 0x8, true}, // smbtorture valid-request value
		{0xFFFF_FFFF, 0x0FFF_FFFF, 0xF, true},
		{0x0000_001F, 1, 0xF, true},
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
	// Bucket 0 is "not tracked"; bucket > LockSequenceIndexMax is out of range.
	c.Store(fid, 0, 1, types.StatusSuccess)
	if c.Len() != 0 {
		t.Fatal("Store with bucket=0 must be ignored")
	}
	c.Store(fid, LockSequenceIndexMax+1, 1, types.StatusSuccess)
	if c.Len() != 0 {
		t.Fatal("Store with out-of-range bucket must be ignored")
	}
	if _, ok := c.Lookup(fid, 0, 1); ok {
		t.Fatal("Lookup with bucket=0 must miss")
	}
	if _, ok := c.Lookup(fid, LockSequenceIndexMax+1, 1); ok {
		t.Fatal("Lookup with out-of-range bucket must miss")
	}
}

func TestLockReplayCache_ForgetFile(t *testing.T) {
	c := NewLockReplayCache()
	fid1 := [16]byte{1}
	fid2 := [16]byte{2}
	c.Store(fid1, 1, 1, types.StatusSuccess)
	c.Store(fid1, 5, 9, types.StatusSuccess)
	c.Store(fid2, 1, 1, types.StatusSuccess)
	c.ForgetFile(fid1)
	if _, ok := c.Lookup(fid1, 1, 1); ok {
		t.Fatal("ForgetFile must drop entries for fid1 bucket 1")
	}
	if _, ok := c.Lookup(fid1, 5, 9); ok {
		t.Fatal("ForgetFile must drop entries for fid1 bucket 5")
	}
	if _, ok := c.Lookup(fid2, 1, 1); !ok {
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
