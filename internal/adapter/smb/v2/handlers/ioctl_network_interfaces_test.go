package handlers

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

func TestEncodeNetworkInterfaceInfoList_Empty(t *testing.T) {
	got := encodeNetworkInterfaceInfoList(nil)
	if got != nil {
		t.Fatalf("expected nil output for empty list, got %d bytes", len(got))
	}
}

func TestEncodeNetworkInterfaceInfoList_IPv4(t *testing.T) {
	entries := []networkInterfaceEntry{{
		IfIndex: 3,
		IP:      net.ParseIP("192.0.2.5").To4(),
	}}

	got := encodeNetworkInterfaceInfoList(entries)
	if len(got) != networkInterfaceInfoEntrySize {
		t.Fatalf("expected %d bytes, got %d", networkInterfaceInfoEntrySize, len(got))
	}

	if next := binary.LittleEndian.Uint32(got[0:4]); next != 0 {
		t.Errorf("Next on only/last entry: got %d, want 0", next)
	}
	if idx := binary.LittleEndian.Uint32(got[4:8]); idx != 3 {
		t.Errorf("IfIndex: got %d, want 3", idx)
	}
	if cap := binary.LittleEndian.Uint32(got[8:12]); cap != 0 {
		t.Errorf("Capability: got 0x%x, want 0", cap)
	}
	if linkSpeed := binary.LittleEndian.Uint64(got[16:24]); linkSpeed != networkInterfaceLinkSpeedBps {
		t.Errorf("LinkSpeed: got %d, want %d", linkSpeed, networkInterfaceLinkSpeedBps)
	}

	sockAddr := got[24:152]
	family := binary.LittleEndian.Uint16(sockAddr[0:2])
	if family != sockAddrFamilyINet {
		t.Errorf("SockAddr family: got 0x%04x, want 0x%04x (AF_INET)", family, sockAddrFamilyINet)
	}
	wantV4 := []byte{192, 0, 2, 5}
	if got := sockAddr[4:8]; !bytes.Equal(got, wantV4) {
		t.Errorf("IPv4 bytes: got %v, want %v", got, wantV4)
	}
}

func TestEncodeNetworkInterfaceInfoList_IPv6(t *testing.T) {
	entries := []networkInterfaceEntry{{
		IfIndex: 7,
		IP:      net.ParseIP("2001:db8::1"),
	}}

	got := encodeNetworkInterfaceInfoList(entries)
	if len(got) != networkInterfaceInfoEntrySize {
		t.Fatalf("expected %d bytes, got %d", networkInterfaceInfoEntrySize, len(got))
	}

	sockAddr := got[24:152]
	family := binary.LittleEndian.Uint16(sockAddr[0:2])
	if family != sockAddrFamilyINet6 {
		t.Errorf("SockAddr family: got 0x%04x, want 0x%04x (AF_INET6)", family, sockAddrFamilyINet6)
	}
	wantV6 := net.ParseIP("2001:db8::1").To16()
	if got := sockAddr[8:24]; !bytes.Equal(got, wantV6) {
		t.Errorf("IPv6 bytes: got %v, want %v", got, wantV6)
	}
}

func TestEncodeNetworkInterfaceInfoList_MultipleEntriesChained(t *testing.T) {
	entries := []networkInterfaceEntry{
		{IfIndex: 1, IP: net.ParseIP("192.0.2.1").To4()},
		{IfIndex: 2, IP: net.ParseIP("192.0.2.2").To4()},
		{IfIndex: 3, IP: net.ParseIP("2001:db8::3")},
	}

	got := encodeNetworkInterfaceInfoList(entries)
	if want := networkInterfaceInfoEntrySize * len(entries); len(got) != want {
		t.Fatalf("expected %d bytes, got %d", want, len(got))
	}

	for i := range entries {
		off := i * networkInterfaceInfoEntrySize
		next := binary.LittleEndian.Uint32(got[off : off+4])
		if i < len(entries)-1 {
			if next != networkInterfaceInfoEntrySize {
				t.Errorf("entry %d Next: got %d, want %d", i, next, networkInterfaceInfoEntrySize)
			}
		} else if next != 0 {
			t.Errorf("last entry Next: got %d, want 0", next)
		}
	}
}

func TestCollectNetworkInterfaceEntries_SkipsLoopback(t *testing.T) {
	entries := collectNetworkInterfaceEntries()
	for _, e := range entries {
		if e.IP.IsLoopback() {
			t.Errorf("loopback address leaked into entries: %v", e.IP)
		}
		if e.IP.IsLinkLocalUnicast() {
			t.Errorf("link-local address leaked into entries: %v", e.IP)
		}
	}
}

// buildQueryNetworkInterfaceInfoRequest builds a 24-byte IOCTL request body
// with the given FileID — the minimal input the handler parses.
func buildQueryNetworkInterfaceInfoRequest(fileID []byte) []byte {
	w := smbenc.NewWriter(24)
	w.WriteUint16(57)
	w.WriteUint16(0)
	w.WriteUint32(FsctlQueryNetworkInterfInfo)
	w.WriteBytes(fileID)
	return w.Bytes()
}

func TestHandleQueryNetworkInterfaceInfo_RejectsNonSentinelFileID(t *testing.T) {
	h := NewHandler()
	ctx := &SMBHandlerContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
	}

	badFileID := make([]byte, 16) // all zero — not the 0xFF sentinel
	body := buildQueryNetworkInterfaceInfoRequest(badFileID)

	result, err := h.Ioctl(ctx, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != types.StatusInvalidParameter {
		t.Errorf("expected StatusInvalidParameter for non-sentinel FileID, got %v", result.Status)
	}
}

func TestHandleQueryNetworkInterfaceInfo_ShortBodyInvalidParameter(t *testing.T) {
	h := NewHandler()
	ctx := &SMBHandlerContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
	}

	// Truncated body (< 24 bytes) — parseIoctlFileID must fail.
	body := make([]byte, 8)
	binary.LittleEndian.PutUint32(body[4:8], FsctlQueryNetworkInterfInfo)

	result, err := h.Ioctl(ctx, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != types.StatusInvalidParameter {
		t.Errorf("expected StatusInvalidParameter for truncated body, got %v", result.Status)
	}
}
