package types

import (
	"bytes"
	"testing"
)

func TestCbNotifyDeviceidArgs_RoundTrip_Change(t *testing.T) {
	devID := DeviceId4{
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
	}
	original := CbNotifyDeviceidArgs{
		Changes: []NotifyDeviceIdChange4{
			{
				Type:      NOTIFY_DEVICEID4_CHANGE,
				DeviceID:  devID,
				Immediate: true,
			},
		},
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded CbNotifyDeviceidArgs
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if len(decoded.Changes) != 1 {
		t.Fatalf("Changes length: got %d, want 1", len(decoded.Changes))
	}
	if decoded.Changes[0].Type != NOTIFY_DEVICEID4_CHANGE {
		t.Errorf("Type: got %d, want %d", decoded.Changes[0].Type, NOTIFY_DEVICEID4_CHANGE)
	}
	if decoded.Changes[0].DeviceID != devID {
		t.Errorf("DeviceID: got %x, want %x", decoded.Changes[0].DeviceID, devID)
	}
	if decoded.Changes[0].Immediate != true {
		t.Errorf("Immediate: got %t, want true", decoded.Changes[0].Immediate)
	}
}

func TestCbNotifyDeviceidArgs_RoundTrip_Delete(t *testing.T) {
	devID := DeviceId4{
		0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x00, 0x11,
		0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99,
	}
	original := CbNotifyDeviceidArgs{
		Changes: []NotifyDeviceIdChange4{
			{
				Type:     NOTIFY_DEVICEID4_DELETE,
				DeviceID: devID,
			},
		},
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded CbNotifyDeviceidArgs
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if len(decoded.Changes) != 1 {
		t.Fatalf("Changes length: got %d, want 1", len(decoded.Changes))
	}
	if decoded.Changes[0].Type != NOTIFY_DEVICEID4_DELETE {
		t.Errorf("Type: got %d, want %d", decoded.Changes[0].Type, NOTIFY_DEVICEID4_DELETE)
	}
	if decoded.Changes[0].DeviceID != devID {
		t.Errorf("DeviceID: got %x, want %x", decoded.Changes[0].DeviceID, devID)
	}
}

func TestCbNotifyDeviceidArgs_RoundTrip_Mixed(t *testing.T) {
	dev1 := DeviceId4{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10}
	dev2 := DeviceId4{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99}
	original := CbNotifyDeviceidArgs{
		Changes: []NotifyDeviceIdChange4{
			{Type: NOTIFY_DEVICEID4_CHANGE, DeviceID: dev1, Immediate: false},
			{Type: NOTIFY_DEVICEID4_DELETE, DeviceID: dev2},
		},
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded CbNotifyDeviceidArgs
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if len(decoded.Changes) != 2 {
		t.Fatalf("Changes length: got %d, want 2", len(decoded.Changes))
	}
	if decoded.Changes[0].Type != NOTIFY_DEVICEID4_CHANGE {
		t.Errorf("Changes[0].Type: got %d, want CHANGE", decoded.Changes[0].Type)
	}
	if decoded.Changes[0].Immediate != false {
		t.Errorf("Changes[0].Immediate: got %t, want false", decoded.Changes[0].Immediate)
	}
	if decoded.Changes[1].Type != NOTIFY_DEVICEID4_DELETE {
		t.Errorf("Changes[1].Type: got %d, want DELETE", decoded.Changes[1].Type)
	}
}

func TestCbNotifyDeviceidRes_RoundTrip(t *testing.T) {
	for _, status := range []uint32{NFS4_OK, NFS4ERR_INVAL} {
		original := CbNotifyDeviceidRes{Status: status}

		var buf bytes.Buffer
		if err := original.Encode(&buf); err != nil {
			t.Fatalf("Encode status=%d: %v", status, err)
		}

		var decoded CbNotifyDeviceidRes
		if err := decoded.Decode(&buf); err != nil {
			t.Fatalf("Decode status=%d: %v", status, err)
		}

		if decoded.Status != status {
			t.Errorf("Status: got %d, want %d", decoded.Status, status)
		}
	}
}

func TestCbNotifyDeviceidArgs_String(t *testing.T) {
	args := CbNotifyDeviceidArgs{Changes: []NotifyDeviceIdChange4{
		{Type: NOTIFY_DEVICEID4_CHANGE},
	}}
	s := args.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}

func TestNotifyDeviceIdChange4_String(t *testing.T) {
	n := NotifyDeviceIdChange4{Type: NOTIFY_DEVICEID4_DELETE}
	s := n.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}
