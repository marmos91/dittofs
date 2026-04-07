package handlers

import (
	"encoding/binary"
	"testing"
)

// SMB2 encoders declared with StructureSize=N must emit (N-1) bytes of fixed
// portion followed by at least one byte of variable section. Empty payloads
// must still emit a single trailing pad byte; otherwise the WPTS Smb2Decoder
// silently drops the response and clients hang on their receive timeout.
// This was the root cause of BVT_SMB2Basic_ChangeNotify_ServerReceiveSmb2Close
// timing out at 10 s; the same latent bug existed in ReadResponse,
// QueryDirectoryResponse, QueryInfoResponse, and buildIoctlResponse. The
// table below pins each encoder to a known empty-payload wire layout so a
// regression in any of them is caught immediately.

func TestEncoders_EmptyVariableSectionPadded(t *testing.T) {
	cases := []struct {
		name        string
		structSize  uint16
		fixedSize   int
		lengthAt    int // offset of the length field that must be 0
		lengthBytes int
		encode      func() []byte
	}{
		{
			name:        "ChangeNotifyResponse",
			structSize:  9,
			fixedSize:   8,
			lengthAt:    4,
			lengthBytes: 4,
			encode: func() []byte {
				body, err := (&ChangeNotifyResponse{}).Encode()
				if err != nil {
					t.Fatalf("ChangeNotifyResponse.Encode: %v", err)
				}
				return body
			},
		},
		{
			name:        "ReadResponse",
			structSize:  17,
			fixedSize:   16,
			lengthAt:    4,
			lengthBytes: 4,
			encode: func() []byte {
				body, err := (&ReadResponse{DataOffset: 80}).Encode()
				if err != nil {
					t.Fatalf("ReadResponse.Encode: %v", err)
				}
				return body
			},
		},
		{
			name:        "QueryDirectoryResponse",
			structSize:  9,
			fixedSize:   8,
			lengthAt:    4,
			lengthBytes: 4,
			encode: func() []byte {
				body, err := (&QueryDirectoryResponse{}).Encode()
				if err != nil {
					t.Fatalf("QueryDirectoryResponse.Encode: %v", err)
				}
				return body
			},
		},
		{
			name:        "QueryInfoResponse",
			structSize:  9,
			fixedSize:   8,
			lengthAt:    4,
			lengthBytes: 4,
			encode: func() []byte {
				body, err := (&QueryInfoResponse{}).Encode()
				if err != nil {
					t.Fatalf("QueryInfoResponse.Encode: %v", err)
				}
				return body
			},
		},
		{
			name:       "IoctlResponse",
			structSize: 49,
			fixedSize:  48,
			// IOCTL layout: StructureSize(2) Reserved(2) CtlCode(4) FileId(16)
			// InputOffset(4) InputCount(4) OutputOffset(4) OutputCount(4)
			// Flags(4) Reserved2(4) Buffer(≥1). OutputCount lives at offset 36.
			lengthAt:    36,
			lengthBytes: 4,
			encode: func() []byte {
				return buildIoctlResponse(0x000900a4, [16]byte{}, nil)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.encode()
			wantLen := tc.fixedSize + 1
			if got := len(body); got != wantLen {
				t.Fatalf("body length = %d, want %d (fixed %d + 1 pad)", got, wantLen, tc.fixedSize)
			}
			if got := binary.LittleEndian.Uint16(body[0:2]); got != tc.structSize {
				t.Errorf("StructureSize = %d, want %d", got, tc.structSize)
			}
			lenField := body[tc.lengthAt : tc.lengthAt+tc.lengthBytes]
			for i, b := range lenField {
				if b != 0 {
					t.Errorf("length field byte %d = %#x, want 0", tc.lengthAt+i, b)
				}
			}
			if pad := body[tc.fixedSize]; pad != 0 {
				t.Errorf("variable-section pad byte = %#x, want 0", pad)
			}
		})
	}
}

func TestChangeNotifyResponse_Encode_NonEmptyKeepsOffset72(t *testing.T) {
	// Spec says OutputBufferOffset is fixed at 72 (header + fixed body) and
	// must NOT regress to 0 just because the buffer is empty — the previous
	// encoder did exactly that and broke WPTS.
	resp := &ChangeNotifyResponse{Buffer: []byte{0xde, 0xad, 0xbe, 0xef}}
	body, err := resp.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if got, want := len(body), 8+4; got != want {
		t.Fatalf("body length = %d, want %d", got, want)
	}
	if got := binary.LittleEndian.Uint16(body[2:4]); got != 72 {
		t.Errorf("OutputBufferOffset = %d, want 72", got)
	}
}
