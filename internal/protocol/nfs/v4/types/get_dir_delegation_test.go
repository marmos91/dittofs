package types

import (
	"bytes"
	"testing"
)

func TestGetDirDelegationArgs_RoundTrip(t *testing.T) {
	original := GetDirDelegationArgs{
		Signal:            true,
		NotificationTypes: Bitmap4{0x0000001f},
		ChildAttrDelay:    NFS4Time{Seconds: 10, Nseconds: 500000000},
		DirAttrDelay:      NFS4Time{Seconds: 30, Nseconds: 0},
		ChildAttrs:        Bitmap4{0x00000001, 0x00000002},
		DirAttrs:          Bitmap4{0x00000003},
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded GetDirDelegationArgs
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Signal != original.Signal {
		t.Errorf("Signal: got %t, want %t", decoded.Signal, original.Signal)
	}
	if len(decoded.NotificationTypes) != len(original.NotificationTypes) {
		t.Errorf("NotificationTypes length: got %d, want %d",
			len(decoded.NotificationTypes), len(original.NotificationTypes))
	}
	if decoded.ChildAttrDelay.Seconds != original.ChildAttrDelay.Seconds {
		t.Errorf("ChildAttrDelay.Seconds: got %d, want %d",
			decoded.ChildAttrDelay.Seconds, original.ChildAttrDelay.Seconds)
	}
	if decoded.ChildAttrDelay.Nseconds != original.ChildAttrDelay.Nseconds {
		t.Errorf("ChildAttrDelay.Nseconds: got %d, want %d",
			decoded.ChildAttrDelay.Nseconds, original.ChildAttrDelay.Nseconds)
	}
	if decoded.DirAttrDelay.Seconds != original.DirAttrDelay.Seconds {
		t.Errorf("DirAttrDelay.Seconds: got %d, want %d",
			decoded.DirAttrDelay.Seconds, original.DirAttrDelay.Seconds)
	}
}

func TestGetDirDelegationRes_RoundTrip_OK(t *testing.T) {
	original := GetDirDelegationRes{
		Status:         NFS4_OK,
		NonFatalStatus: GDD4_OK,
		OK: &GetDirDelegationResOK{
			CookieVerf:        [8]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08},
			Stateid:           ValidStateid(),
			NotificationTypes: Bitmap4{0x0000001f},
			ChildAttrs:        Bitmap4{0x00000001},
			DirAttrs:          Bitmap4{0x00000002},
		},
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded GetDirDelegationRes
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Status != NFS4_OK {
		t.Fatalf("Status: got %d, want OK", decoded.Status)
	}
	if decoded.NonFatalStatus != GDD4_OK {
		t.Fatalf("NonFatalStatus: got %d, want GDD4_OK", decoded.NonFatalStatus)
	}
	if decoded.OK == nil {
		t.Fatal("OK should not be nil")
	}
	if decoded.OK.CookieVerf != original.OK.CookieVerf {
		t.Errorf("CookieVerf: got %x, want %x", decoded.OK.CookieVerf, original.OK.CookieVerf)
	}
	if decoded.OK.Stateid.Seqid != original.OK.Stateid.Seqid {
		t.Errorf("Stateid.Seqid: got %d, want %d", decoded.OK.Stateid.Seqid, original.OK.Stateid.Seqid)
	}
}

func TestGetDirDelegationRes_RoundTrip_Unavail(t *testing.T) {
	original := GetDirDelegationRes{
		Status:               NFS4_OK,
		NonFatalStatus:       GDD4_UNAVAIL,
		WillSignalDelegAvail: true,
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded GetDirDelegationRes
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Status != NFS4_OK {
		t.Fatalf("Status: got %d, want OK", decoded.Status)
	}
	if decoded.NonFatalStatus != GDD4_UNAVAIL {
		t.Fatalf("NonFatalStatus: got %d, want GDD4_UNAVAIL", decoded.NonFatalStatus)
	}
	if decoded.WillSignalDelegAvail != true {
		t.Errorf("WillSignalDelegAvail: got %t, want true", decoded.WillSignalDelegAvail)
	}
}

func TestGetDirDelegationRes_RoundTrip_Error(t *testing.T) {
	original := GetDirDelegationRes{Status: NFS4ERR_NOTSUPP}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded GetDirDelegationRes
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Status != NFS4ERR_NOTSUPP {
		t.Errorf("Status: got %d, want %d", decoded.Status, NFS4ERR_NOTSUPP)
	}
}

func TestGetDirDelegationArgs_String(t *testing.T) {
	args := GetDirDelegationArgs{
		Signal:            true,
		NotificationTypes: Bitmap4{0x01},
		ChildAttrDelay:    NFS4Time{Seconds: 5},
		DirAttrDelay:      NFS4Time{Seconds: 10},
		ChildAttrs:        Bitmap4{},
		DirAttrs:          Bitmap4{},
	}
	s := args.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}
