package types

import (
	"bytes"
	"testing"
)

func TestGetDeviceInfoArgs_RoundTrip(t *testing.T) {
	original := GetDeviceInfoArgs{
		DeviceID:    DeviceId4{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10},
		LayoutType:  LAYOUT4_NFSV4_1_FILES,
		MaxCount:    65536,
		NotifyTypes: Bitmap4{0x00000001},
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded GetDeviceInfoArgs
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.DeviceID != original.DeviceID {
		t.Errorf("DeviceID: got %x, want %x", decoded.DeviceID, original.DeviceID)
	}
	if decoded.LayoutType != original.LayoutType {
		t.Errorf("LayoutType: got %d, want %d", decoded.LayoutType, original.LayoutType)
	}
	if decoded.MaxCount != original.MaxCount {
		t.Errorf("MaxCount: got %d, want %d", decoded.MaxCount, original.MaxCount)
	}
	if len(decoded.NotifyTypes) != len(original.NotifyTypes) {
		t.Errorf("NotifyTypes length: got %d, want %d",
			len(decoded.NotifyTypes), len(original.NotifyTypes))
	}
}

func TestGetDeviceInfoRes_RoundTrip_OK(t *testing.T) {
	original := GetDeviceInfoRes{
		Status:       NFS4_OK,
		LayoutType:   LAYOUT4_NFSV4_1_FILES,
		DeviceAddr:   []byte{0xaa, 0xbb, 0xcc, 0xdd},
		Notification: Bitmap4{0x00000003},
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded GetDeviceInfoRes
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Status != NFS4_OK {
		t.Fatalf("Status: got %d, want OK", decoded.Status)
	}
	if decoded.LayoutType != LAYOUT4_NFSV4_1_FILES {
		t.Errorf("LayoutType: got %d, want %d", decoded.LayoutType, LAYOUT4_NFSV4_1_FILES)
	}
	if !bytes.Equal(decoded.DeviceAddr, original.DeviceAddr) {
		t.Errorf("DeviceAddr: got %x, want %x", decoded.DeviceAddr, original.DeviceAddr)
	}
}

func TestGetDeviceInfoRes_RoundTrip_TooSmall(t *testing.T) {
	original := GetDeviceInfoRes{
		Status:   NFS4ERR_TOOSMALL,
		MinCount: 131072,
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded GetDeviceInfoRes
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Status != NFS4ERR_TOOSMALL {
		t.Fatalf("Status: got %d, want %d", decoded.Status, NFS4ERR_TOOSMALL)
	}
	if decoded.MinCount != 131072 {
		t.Errorf("MinCount: got %d, want 131072", decoded.MinCount)
	}
}

func TestGetDeviceInfoRes_RoundTrip_Error(t *testing.T) {
	original := GetDeviceInfoRes{Status: NFS4ERR_NOTSUPP}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded GetDeviceInfoRes
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Status != NFS4ERR_NOTSUPP {
		t.Errorf("Status: got %d, want %d", decoded.Status, NFS4ERR_NOTSUPP)
	}
}

func TestGetDeviceInfoArgs_String(t *testing.T) {
	args := GetDeviceInfoArgs{LayoutType: LAYOUT4_NFSV4_1_FILES, MaxCount: 1024}
	s := args.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}
