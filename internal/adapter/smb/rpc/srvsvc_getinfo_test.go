package rpc

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// encodeUniqueString builds the NDR wire form of a unique-pointer-prefixed
// conformant/varying wide string, matching what a client sends for the
// [unique] ServerName argument of NetrShareGetInfo.
func encodeUniqueString(referent uint32, s string) []byte {
	buf := appendUint32(nil, referent)
	return appendConformantVaryingString(buf, s)
}

// encodeGetInfoRequest builds a NetrShareGetInfo request stub in real Windows
// wire order: a [unique] ServerName pointer (referent, then its inline string
// when non-null), the [ref] NetName string inline with no referent, then Level.
func encodeGetInfoRequest(serverReferent uint32, serverName, netName string, level uint32) []byte {
	var stub []byte
	if serverReferent == 0 {
		stub = appendUint32(nil, 0) // null ServerName unique pointer
	} else {
		stub = encodeUniqueString(serverReferent, serverName)
	}
	stub = appendConformantVaryingString(stub, netName) // NetName: inline, no referent
	return appendUint32(stub, level)
}

func TestParseShareGetInfoRequest(t *testing.T) {
	stub := encodeGetInfoRequest(0x00020000, "\\\\SERVER", "export", 502)

	name, level, ok := parseShareGetInfoRequest(stub)
	if !ok {
		t.Fatal("parseShareGetInfoRequest: unexpected failure")
	}
	if name != "export" {
		t.Errorf("share name = %q, want %q", name, "export")
	}
	if level != 502 {
		t.Errorf("level = %d, want 502", level)
	}
}

// TestConformantVaryingStringRoundTrip_NonASCII guards the NDR count against the
// UTF-8-bytes-vs-UTF-16-units mismatch: the encoded MaxCount/ActualCount must
// match what the reader consumes, or every following field misaligns.
func TestConformantVaryingStringRoundTrip_NonASCII(t *testing.T) {
	for _, s := range []string{"café", "共有", "plain"} {
		buf := appendConformantVaryingString(nil, s)
		got, next, ok := readConformantVaryingString(buf, 0)
		if !ok {
			t.Fatalf("readConformantVaryingString(%q): failed", s)
		}
		if got != s {
			t.Errorf("round-trip = %q, want %q", got, s)
		}
		if next != len(buf) {
			t.Errorf("reader consumed %d bytes, encoder wrote %d for %q", next, len(buf), s)
		}
	}
}

func TestParseShareGetInfoRequest_NullServerName(t *testing.T) {
	stub := encodeGetInfoRequest(0, "", "data", 502)

	name, level, ok := parseShareGetInfoRequest(stub)
	if !ok || name != "data" || level != 502 {
		t.Fatalf("parse = (%q, %d, %v), want (data, 502, true)", name, level, ok)
	}
}

// TestNetrShareGetInfo502RoundTrip drives a full NetrShareGetInfo request through
// the handler and decodes the response, verifying the SHARE_INFO_502 carries the
// share's netname, remark, and security descriptor intact.
func TestNetrShareGetInfo502RoundTrip(t *testing.T) {
	sd := []byte{0x01, 0x00, 0x14, 0x80, 0xDE, 0xAD, 0xBE, 0xEF, 0x11, 0x22} // opaque SD bytes
	h := NewSRVSVCHandler([]ShareInfo1{{
		Name:               "export",
		Type:               STYPE_DISKTREE,
		Comment:            "DittoFS share",
		SecurityDescriptor: sd,
	}})

	stub := encodeGetInfoRequest(0x00020000, "\\\\SERVER", "export", 502)

	req := &Request{
		OpNum:     OpNetrShareGetInfo,
		ContextID: 0,
		StubData:  stub,
		Header:    Header{CallID: 7},
	}

	resp := h.HandleRequest(req)
	// Response PDU: 16-byte header + alloc_hint(4) + context(2) + cancel(1) +
	// reserved(1), so the NDR stub starts at offset 24.
	if len(resp) < 24 {
		t.Fatalf("response too short: %d bytes", len(resp))
	}
	body := resp[24:]

	if got := binary.LittleEndian.Uint32(body[0:]); got != 502 {
		t.Errorf("Level = %d, want 502", got)
	}
	if got := binary.LittleEndian.Uint32(body[4:]); got != 502 {
		t.Errorf("union discriminant = %d, want 502", got)
	}
	if got := binary.LittleEndian.Uint32(body[8:]); got == 0 {
		t.Error("SHARE_INFO_502 referent is null, want non-null")
	}

	// SHARE_INFO_502 fixed part begins at offset 12.
	reserved := binary.LittleEndian.Uint32(body[12+32:]) // reserved is the 9th DWORD
	if int(reserved) != len(sd) {
		t.Errorf("shi502_reserved = %d, want SD length %d", reserved, len(sd))
	}

	// Deferred members start after the 10-DWORD fixed part (offset 12 + 40 = 52):
	// netname, remark, then the SD conformant array.
	pos := 12 + 40
	netname, pos, ok := readConformantVaryingString(body, pos)
	if !ok || netname != "export" {
		t.Fatalf("netname = %q (ok=%v), want %q", netname, ok, "export")
	}
	remark, pos, ok := readConformantVaryingString(body, pos)
	if !ok || remark != "DittoFS share" {
		t.Fatalf("remark = %q (ok=%v), want %q", remark, ok, "DittoFS share")
	}

	// SD conformant array: max count then the raw bytes.
	maxCount := binary.LittleEndian.Uint32(body[pos:])
	pos += 4
	if int(maxCount) != len(sd) {
		t.Fatalf("SD array max count = %d, want %d", maxCount, len(sd))
	}
	if got := body[pos : pos+len(sd)]; !bytes.Equal(got, sd) {
		t.Errorf("SD bytes = %x, want %x", got, sd)
	}
}

func TestNetrShareGetInfo_UnknownShare(t *testing.T) {
	h := NewSRVSVCHandler([]ShareInfo1{{Name: "export", Type: STYPE_DISKTREE}})

	stub := encodeGetInfoRequest(0, "", "missing", 502)

	resp := h.HandleRequest(&Request{OpNum: OpNetrShareGetInfo, StubData: stub, Header: Header{CallID: 1}})
	body := resp[24:]
	// Error form: Level, discriminant, null arm pointer, status.
	if status := binary.LittleEndian.Uint32(body[12:]); status != NERR_NetNameNotFound {
		t.Errorf("status = %#x, want NERR_NetNameNotFound (%#x)", status, NERR_NetNameNotFound)
	}
}

func TestNetrShareGetInfo_UnsupportedLevel(t *testing.T) {
	h := NewSRVSVCHandler([]ShareInfo1{{Name: "export", Type: STYPE_DISKTREE}})

	stub := encodeGetInfoRequest(0, "", "export", 2) // level 2, unsupported

	resp := h.HandleRequest(&Request{OpNum: OpNetrShareGetInfo, StubData: stub, Header: Header{CallID: 1}})
	body := resp[24:]
	if status := binary.LittleEndian.Uint32(body[12:]); status != ERROR_INVALID_LEVEL {
		t.Errorf("status = %#x, want ERROR_INVALID_LEVEL (%#x)", status, ERROR_INVALID_LEVEL)
	}
}
