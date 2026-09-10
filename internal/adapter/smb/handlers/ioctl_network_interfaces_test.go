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

// buildQueryNetworkInterfaceInfoRequestWithMaxOutput builds a full 56-byte
// IOCTL envelope (MS-SMB2 2.2.31) with a configurable MaxOutputResponse so
// the BUFFER_TOO_SMALL path required by smbtorture bug14788.NETWORK_INTERFACE
// (sentinel FileID + max_output=1) can be exercised in isolation.
func buildQueryNetworkInterfaceInfoRequestWithMaxOutput(fileID []byte, maxOutput uint32) []byte {
	w := smbenc.NewWriter(56)
	w.WriteUint16(57)
	w.WriteUint16(0)
	w.WriteUint32(FsctlQueryNetworkInterfInfo)
	w.WriteBytes(fileID)
	w.WriteUint32(0) // InputOffset
	w.WriteUint32(0) // InputCount
	w.WriteUint32(0) // MaxInputResponse
	w.WriteUint32(0) // OutputOffset
	w.WriteUint32(0) // OutputCount
	w.WriteUint32(maxOutput)
	w.WriteUint32(0) // Flags
	w.WriteUint32(0) // Reserved2
	return w.Bytes()
}

// TestHandleQueryNetworkInterfaceInfo_BufferTooSmall pins step 2 of
// smbtorture bug14788.NETWORK_INTERFACE: sentinel FileID + max_output=1 must
// be STATUS_BUFFER_TOO_SMALL (distinct from INVALID_PARAMETER, which is the
// non-sentinel-FileID outcome — that ordering is what bug14788 specifically
// regression-tests).
func TestHandleQueryNetworkInterfaceInfo_BufferTooSmall(t *testing.T) {
	// The handler short-circuits with NOT_SUPPORTED when the host has no
	// advertisable interfaces — that's correct behaviour but not what we
	// want to assert here.
	if len(collectNetworkInterfaceEntries()) == 0 {
		t.Skip("host has no advertisable interfaces; BUFFER_TOO_SMALL gate unreachable")
	}

	h := NewHandler()
	ctx := &SMBHandlerContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
	}
	body := buildQueryNetworkInterfaceInfoRequestWithMaxOutput(allFFFileID, 1)

	result, err := h.Ioctl(ctx, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != types.StatusBufferTooSmall {
		t.Errorf("expected StatusBufferTooSmall for sentinel FileID + max_output=1, got %v",
			result.Status)
	}
}

// TestHandleQueryNetworkInterfaceInfo_NonSentinelFileIDInvalidParameter pins
// step 3 of bug14788.NETWORK_INTERFACE: a non-sentinel FileID must yield
// INVALID_PARAMETER even when max_output is small. This is the property
// that distinguishes "bad FileID" from "buffer issue".
func TestHandleQueryNetworkInterfaceInfo_NonSentinelFileIDInvalidParameter(t *testing.T) {
	h := NewHandler()
	ctx := &SMBHandlerContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
	}
	// INT64_MAX persistent/volatile pair from smbtorture — the high bytes
	// are zero so the 16-byte ID does NOT match the all-0xFF sentinel.
	badFileID := make([]byte, 16)
	binary.LittleEndian.PutUint64(badFileID[0:8], 0x7FFFFFFFFFFFFFFF)
	binary.LittleEndian.PutUint64(badFileID[8:16], 0x7FFFFFFFFFFFFFFF)
	body := buildQueryNetworkInterfaceInfoRequestWithMaxOutput(badFileID, 1)

	result, err := h.Ioctl(ctx, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != types.StatusInvalidParameter {
		t.Errorf("expected StatusInvalidParameter (not BUFFER_TOO_SMALL) for non-sentinel FileID, got %v",
			result.Status)
	}
}
