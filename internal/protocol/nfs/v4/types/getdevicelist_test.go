package types

import (
	"bytes"
	"testing"
)

func TestGetDeviceListArgs_RoundTrip(t *testing.T) {
	original := GetDeviceListArgs{
		LayoutType: LAYOUT4_NFSV4_1_FILES,
		MaxDevices: 100,
		Cookie:     42,
		CookieVerf: [8]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08},
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded GetDeviceListArgs
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.LayoutType != original.LayoutType {
		t.Errorf("LayoutType: got %d, want %d", decoded.LayoutType, original.LayoutType)
	}
	if decoded.MaxDevices != original.MaxDevices {
		t.Errorf("MaxDevices: got %d, want %d", decoded.MaxDevices, original.MaxDevices)
	}
	if decoded.Cookie != original.Cookie {
		t.Errorf("Cookie: got %d, want %d", decoded.Cookie, original.Cookie)
	}
	if decoded.CookieVerf != original.CookieVerf {
		t.Errorf("CookieVerf: got %x, want %x", decoded.CookieVerf, original.CookieVerf)
	}
}

func TestGetDeviceListRes_RoundTrip_OK(t *testing.T) {
	dev1 := DeviceId4{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10}
	dev2 := DeviceId4{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0x00}

	original := GetDeviceListRes{
		Status:     NFS4_OK,
		Cookie:     99,
		CookieVerf: [8]byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x11, 0x22},
		DeviceIDs:  []DeviceId4{dev1, dev2},
		EOF:        true,
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded GetDeviceListRes
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Status != NFS4_OK {
		t.Fatalf("Status: got %d, want OK", decoded.Status)
	}
	if decoded.Cookie != 99 {
		t.Errorf("Cookie: got %d, want 99", decoded.Cookie)
	}
	if decoded.CookieVerf != original.CookieVerf {
		t.Errorf("CookieVerf: got %x, want %x", decoded.CookieVerf, original.CookieVerf)
	}
	if len(decoded.DeviceIDs) != 2 {
		t.Fatalf("DeviceIDs length: got %d, want 2", len(decoded.DeviceIDs))
	}
	if decoded.DeviceIDs[0] != dev1 {
		t.Errorf("DeviceIDs[0]: got %x, want %x", decoded.DeviceIDs[0], dev1)
	}
	if decoded.DeviceIDs[1] != dev2 {
		t.Errorf("DeviceIDs[1]: got %x, want %x", decoded.DeviceIDs[1], dev2)
	}
	if decoded.EOF != true {
		t.Errorf("EOF: got %t, want true", decoded.EOF)
	}
}

func TestGetDeviceListRes_RoundTrip_Empty(t *testing.T) {
	original := GetDeviceListRes{
		Status:    NFS4_OK,
		DeviceIDs: []DeviceId4{},
		EOF:       true,
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded GetDeviceListRes
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if len(decoded.DeviceIDs) != 0 {
		t.Errorf("DeviceIDs should be empty, got %d", len(decoded.DeviceIDs))
	}
	if decoded.EOF != true {
		t.Errorf("EOF: got %t, want true", decoded.EOF)
	}
}

func TestGetDeviceListRes_RoundTrip_Error(t *testing.T) {
	original := GetDeviceListRes{Status: NFS4ERR_NOTSUPP}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded GetDeviceListRes
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Status != NFS4ERR_NOTSUPP {
		t.Errorf("Status: got %d, want %d", decoded.Status, NFS4ERR_NOTSUPP)
	}
}

func TestGetDeviceListArgs_String(t *testing.T) {
	args := GetDeviceListArgs{LayoutType: LAYOUT4_NFSV4_1_FILES, MaxDevices: 50}
	s := args.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}
