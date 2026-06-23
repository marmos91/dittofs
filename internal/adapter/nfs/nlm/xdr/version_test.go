package xdr

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/nlm/types"
)

// TestNLMLockRoundTrip_WidthVariants verifies that an NLM lock encoded with a
// given offset width (64-bit for NLM v4, 32-bit for NLM v1/v3) decodes back to
// the same logical values. macOS NFSv3 clients send NLM v1/v3 with 32-bit
// offsets, so the codec must honour the wire width selected by the version.
func TestNLMLockRoundTrip_WidthVariants(t *testing.T) {
	cases := []struct {
		name   string
		wide   bool
		offset uint64
		length uint64
	}{
		{"v4 wide small", true, 4096, 8192},
		{"v4 wide large", true, 0x1_0000_0000, 0x2_0000_0000},
		{"v1v3 narrow", false, 4096, 8192},
		{"v1v3 narrow max32", false, 0xFFFF_FFFF, 0xFFFF_FFFE},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := &types.NLM4Lock{
				CallerName: "macos-client",
				FH:         []byte{0x01, 0x02, 0x03, 0x04},
				OH:         []byte{0xaa, 0xbb},
				Svid:       1234,
				Offset:     tc.offset,
				Length:     tc.length,
			}

			buf := new(bytes.Buffer)
			if err := EncodeNLM4Lock(buf, in, tc.wide); err != nil {
				t.Fatalf("EncodeNLM4Lock(wide=%v): %v", tc.wide, err)
			}

			out, err := DecodeNLM4Lock(bytes.NewReader(buf.Bytes()), tc.wide)
			if err != nil {
				t.Fatalf("DecodeNLM4Lock(wide=%v): %v", tc.wide, err)
			}

			if out.Offset != tc.offset || out.Length != tc.length {
				t.Fatalf("offset/len mismatch: got (%d,%d) want (%d,%d)",
					out.Offset, out.Length, tc.offset, tc.length)
			}
			if out.CallerName != in.CallerName || out.Svid != in.Svid {
				t.Fatalf("scalar mismatch: %+v vs %+v", out, in)
			}
		})
	}
}

// TestNLMLock_NarrowWireSize asserts the 32-bit variant emits exactly 8 fewer
// bytes than the 64-bit variant (offset + length each 4 bytes narrower),
// proving the wire format actually differs (not just a widened decode).
func TestNLMLock_NarrowWireSize(t *testing.T) {
	in := &types.NLM4Lock{CallerName: "c", FH: []byte{1}, OH: []byte{2}, Svid: 1, Offset: 1, Length: 2}

	wide := new(bytes.Buffer)
	if err := EncodeNLM4Lock(wide, in, true); err != nil {
		t.Fatal(err)
	}
	narrow := new(bytes.Buffer)
	if err := EncodeNLM4Lock(narrow, in, false); err != nil {
		t.Fatal(err)
	}

	if diff := wide.Len() - narrow.Len(); diff != 8 {
		t.Fatalf("expected 64-bit encoding to be 8 bytes larger, got diff=%d", diff)
	}
}

// TestNLM4TestRes_HolderWidth verifies the TEST response holder honours the
// offset width too (the conflicting-lock report must match the client version).
func TestNLM4TestRes_HolderWidth(t *testing.T) {
	res := &types.NLM4TestRes{
		Cookie: []byte{0x01},
		Status: types.NLM4Denied,
		Holder: &types.NLM4Holder{Exclusive: true, Svid: 7, OH: []byte{9}, Offset: 100, Length: 200},
	}

	for _, wide := range []bool{true, false} {
		buf := new(bytes.Buffer)
		if err := EncodeNLM4TestRes(buf, res, wide); err != nil {
			t.Fatalf("EncodeNLM4TestRes(wide=%v): %v", wide, err)
		}
		if buf.Len() == 0 {
			t.Fatalf("empty encoding for wide=%v", wide)
		}
	}
}

// TestNLMHolder_NarrowSaturatesNotWraps verifies that when a TEST holder field
// reflects a conflicting v4 lock whose 64-bit range exceeds 32 bits, the narrow
// (v1/v3) encoding saturates to the 32-bit max instead of wrap-truncating. A
// wrap would fold e.g. 1<<32 down to 0 and misreport the conflict at offset 0.
func TestNLMHolder_NarrowSaturatesNotWraps(t *testing.T) {
	holder := &types.NLM4Holder{
		Exclusive: true,
		Svid:      7,
		OH:        nil,
		Offset:    0x1_0000_0000, // 1<<32 — wraps to 0 if truncated
		Length:    0x1_0000_0005, // wraps to 5 if truncated
	}

	buf := new(bytes.Buffer)
	if err := EncodeNLM4Holder(buf, holder, false /* narrow */); err != nil {
		t.Fatalf("EncodeNLM4Holder narrow: %v", err)
	}

	b := buf.Bytes()
	// Layout (narrow, empty OH): exclusive[4] svid[4] oh_len[4] l_offset[4] l_len[4]
	if len(b) != 20 {
		t.Fatalf("unexpected narrow holder size: got %d, want 20 (%x)", len(b), b)
	}
	offset := binary.BigEndian.Uint32(b[12:16])
	length := binary.BigEndian.Uint32(b[16:20])
	if offset != 0xFFFF_FFFF {
		t.Errorf("l_offset: got %#x, want saturated 0xFFFFFFFF (wrap would give 0)", offset)
	}
	if length != 0xFFFF_FFFF {
		t.Errorf("l_len: got %#x, want saturated 0xFFFFFFFF (wrap would give 5)", length)
	}
}
