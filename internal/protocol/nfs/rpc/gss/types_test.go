package gss

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestDecodeGSSCred_INIT(t *testing.T) {
	// Build an INIT credential: version=1, gss_proc=INIT, seq_num=0, service=none, handle empty
	buf := &bytes.Buffer{}
	_ = binary.Write(buf, binary.BigEndian, RPCGSSVers1)   // version
	_ = binary.Write(buf, binary.BigEndian, RPCGSSInit)    // gss_proc
	_ = binary.Write(buf, binary.BigEndian, uint32(0))     // seq_num
	_ = binary.Write(buf, binary.BigEndian, RPCGSSSvcNone) // service
	_ = binary.Write(buf, binary.BigEndian, uint32(0))     // handle length (empty)

	cred, err := DecodeGSSCred(buf.Bytes())
	if err != nil {
		t.Fatalf("DecodeGSSCred failed: %v", err)
	}

	if cred.GSSProc != RPCGSSInit {
		t.Errorf("GSSProc = %d, want %d", cred.GSSProc, RPCGSSInit)
	}
	if cred.SeqNum != 0 {
		t.Errorf("SeqNum = %d, want 0", cred.SeqNum)
	}
	if cred.Service != RPCGSSSvcNone {
		t.Errorf("Service = %d, want %d", cred.Service, RPCGSSSvcNone)
	}
	if len(cred.Handle) != 0 {
		t.Errorf("Handle length = %d, want 0", len(cred.Handle))
	}
}

func TestDecodeGSSCred_DATA(t *testing.T) {
	// Build a DATA credential with a non-empty handle
	handle := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0xCA, 0xFE}
	buf := &bytes.Buffer{}
	_ = binary.Write(buf, binary.BigEndian, RPCGSSVers1)         // version
	_ = binary.Write(buf, binary.BigEndian, RPCGSSData)          // gss_proc
	_ = binary.Write(buf, binary.BigEndian, uint32(42))          // seq_num
	_ = binary.Write(buf, binary.BigEndian, RPCGSSSvcIntegrity)  // service
	_ = binary.Write(buf, binary.BigEndian, uint32(len(handle))) // handle length
	buf.Write(handle)
	// XDR padding: 6 bytes -> padding = (4 - 6%4) % 4 = 2
	buf.Write([]byte{0, 0})

	cred, err := DecodeGSSCred(buf.Bytes())
	if err != nil {
		t.Fatalf("DecodeGSSCred failed: %v", err)
	}

	if cred.GSSProc != RPCGSSData {
		t.Errorf("GSSProc = %d, want %d", cred.GSSProc, RPCGSSData)
	}
	if cred.SeqNum != 42 {
		t.Errorf("SeqNum = %d, want 42", cred.SeqNum)
	}
	if cred.Service != RPCGSSSvcIntegrity {
		t.Errorf("Service = %d, want %d", cred.Service, RPCGSSSvcIntegrity)
	}
	if !bytes.Equal(cred.Handle, handle) {
		t.Errorf("Handle = %x, want %x", cred.Handle, handle)
	}
}

func TestDecodeGSSCred_RejectsInvalidVersion(t *testing.T) {
	buf := &bytes.Buffer{}
	_ = binary.Write(buf, binary.BigEndian, uint32(2))     // version=2 (invalid)
	_ = binary.Write(buf, binary.BigEndian, RPCGSSData)    // gss_proc
	_ = binary.Write(buf, binary.BigEndian, uint32(1))     // seq_num
	_ = binary.Write(buf, binary.BigEndian, RPCGSSSvcNone) // service
	_ = binary.Write(buf, binary.BigEndian, uint32(0))     // handle length

	_, err := DecodeGSSCred(buf.Bytes())
	if err == nil {
		t.Fatal("DecodeGSSCred should reject version != 1")
	}
}

func TestDecodeGSSCred_RejectsShortBody(t *testing.T) {
	// Body too short (less than 20 bytes minimum)
	_, err := DecodeGSSCred([]byte{0, 0, 0, 1})
	if err == nil {
		t.Fatal("DecodeGSSCred should reject short body")
	}
}

func TestEncodeGSSCred_Roundtrip(t *testing.T) {
	tests := []struct {
		name string
		cred RPCGSSCredV1
	}{
		{
			name: "INIT with empty handle",
			cred: RPCGSSCredV1{
				GSSProc: RPCGSSInit,
				SeqNum:  0,
				Service: RPCGSSSvcNone,
				Handle:  nil,
			},
		},
		{
			name: "DATA with handle",
			cred: RPCGSSCredV1{
				GSSProc: RPCGSSData,
				SeqNum:  100,
				Service: RPCGSSSvcIntegrity,
				Handle:  []byte{0x01, 0x02, 0x03, 0x04, 0x05},
			},
		},
		{
			name: "DESTROY with handle",
			cred: RPCGSSCredV1{
				GSSProc: RPCGSSDestroy,
				SeqNum:  999,
				Service: RPCGSSSvcPrivacy,
				Handle:  []byte{0xAA, 0xBB, 0xCC, 0xDD},
			},
		},
		{
			name: "DATA with 4-byte aligned handle",
			cred: RPCGSSCredV1{
				GSSProc: RPCGSSData,
				SeqNum:  50,
				Service: RPCGSSSvcNone,
				Handle:  []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded, err := EncodeGSSCred(&tt.cred)
			if err != nil {
				t.Fatalf("EncodeGSSCred failed: %v", err)
			}

			decoded, err := DecodeGSSCred(encoded)
			if err != nil {
				t.Fatalf("DecodeGSSCred failed: %v", err)
			}

			if decoded.GSSProc != tt.cred.GSSProc {
				t.Errorf("GSSProc = %d, want %d", decoded.GSSProc, tt.cred.GSSProc)
			}
			if decoded.SeqNum != tt.cred.SeqNum {
				t.Errorf("SeqNum = %d, want %d", decoded.SeqNum, tt.cred.SeqNum)
			}
			if decoded.Service != tt.cred.Service {
				t.Errorf("Service = %d, want %d", decoded.Service, tt.cred.Service)
			}

			// Compare handles (nil and empty []byte are equivalent)
			if len(decoded.Handle) != len(tt.cred.Handle) {
				t.Errorf("Handle length = %d, want %d", len(decoded.Handle), len(tt.cred.Handle))
			} else if len(tt.cred.Handle) > 0 && !bytes.Equal(decoded.Handle, tt.cred.Handle) {
				t.Errorf("Handle = %x, want %x", decoded.Handle, tt.cred.Handle)
			}
		})
	}
}

func TestEncodeGSSInitRes(t *testing.T) {
	res := &RPCGSSInitRes{
		Handle:    []byte{0x01, 0x02, 0x03},
		GSSMajor:  GSSComplete,
		GSSMinor:  0,
		SeqWindow: 128,
		GSSToken:  []byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE},
	}

	encoded, err := EncodeGSSInitRes(res)
	if err != nil {
		t.Fatalf("EncodeGSSInitRes failed: %v", err)
	}

	// Verify the encoded data can be parsed back
	reader := bytes.NewReader(encoded)

	// Read handle
	var handleLen uint32
	_ = binary.Read(reader, binary.BigEndian, &handleLen)
	if handleLen != 3 {
		t.Errorf("handle length = %d, want 3", handleLen)
	}
	handle := make([]byte, handleLen)
	_, _ = reader.Read(handle)
	if !bytes.Equal(handle, res.Handle) {
		t.Errorf("handle = %x, want %x", handle, res.Handle)
	}
	// Skip padding (3 bytes -> padding = 1)
	_, _ = reader.ReadByte()

	// Read gss_major
	var gssMajor uint32
	_ = binary.Read(reader, binary.BigEndian, &gssMajor)
	if gssMajor != GSSComplete {
		t.Errorf("gss_major = %d, want %d", gssMajor, GSSComplete)
	}

	// Read gss_minor
	var gssMinor uint32
	_ = binary.Read(reader, binary.BigEndian, &gssMinor)
	if gssMinor != 0 {
		t.Errorf("gss_minor = %d, want 0", gssMinor)
	}

	// Read seq_window
	var seqWindow uint32
	_ = binary.Read(reader, binary.BigEndian, &seqWindow)
	if seqWindow != 128 {
		t.Errorf("seq_window = %d, want 128", seqWindow)
	}

	// Read gss_token
	var tokenLen uint32
	_ = binary.Read(reader, binary.BigEndian, &tokenLen)
	if tokenLen != 5 {
		t.Errorf("token length = %d, want 5", tokenLen)
	}
	token := make([]byte, tokenLen)
	_, _ = reader.Read(token)
	if !bytes.Equal(token, res.GSSToken) {
		t.Errorf("token = %x, want %x", token, res.GSSToken)
	}
}

func TestEncodeGSSInitRes_EmptyFields(t *testing.T) {
	res := &RPCGSSInitRes{
		Handle:    nil,
		GSSMajor:  GSSContinueNeeded,
		GSSMinor:  42,
		SeqWindow: 0,
		GSSToken:  nil,
	}

	encoded, err := EncodeGSSInitRes(res)
	if err != nil {
		t.Fatalf("EncodeGSSInitRes failed: %v", err)
	}

	reader := bytes.NewReader(encoded)

	// Handle should be zero-length opaque
	var handleLen uint32
	_ = binary.Read(reader, binary.BigEndian, &handleLen)
	if handleLen != 0 {
		t.Errorf("handle length = %d, want 0", handleLen)
	}

	var gssMajor uint32
	_ = binary.Read(reader, binary.BigEndian, &gssMajor)
	if gssMajor != GSSContinueNeeded {
		t.Errorf("gss_major = %d, want %d", gssMajor, GSSContinueNeeded)
	}

	var gssMinor uint32
	_ = binary.Read(reader, binary.BigEndian, &gssMinor)
	if gssMinor != 42 {
		t.Errorf("gss_minor = %d, want 42", gssMinor)
	}

	var seqWindow uint32
	_ = binary.Read(reader, binary.BigEndian, &seqWindow)
	if seqWindow != 0 {
		t.Errorf("seq_window = %d, want 0", seqWindow)
	}

	// Token should be zero-length opaque
	var tokenLen uint32
	_ = binary.Read(reader, binary.BigEndian, &tokenLen)
	if tokenLen != 0 {
		t.Errorf("token length = %d, want 0", tokenLen)
	}
}

func TestConstants(t *testing.T) {
	// Verify constants match RFC specifications
	if AuthRPCSECGSS != 6 {
		t.Errorf("AuthRPCSECGSS = %d, want 6", AuthRPCSECGSS)
	}
	if MAXSEQ != 0x80000000 {
		t.Errorf("MAXSEQ = %d, want %d", MAXSEQ, 0x80000000)
	}
	if PseudoFlavorKrb5 != 390003 {
		t.Errorf("PseudoFlavorKrb5 = %d, want 390003", PseudoFlavorKrb5)
	}
	if PseudoFlavorKrb5i != 390004 {
		t.Errorf("PseudoFlavorKrb5i = %d, want 390004", PseudoFlavorKrb5i)
	}
	if PseudoFlavorKrb5p != 390005 {
		t.Errorf("PseudoFlavorKrb5p = %d, want 390005", PseudoFlavorKrb5p)
	}

	// Key usage per RFC 4121 Section 2
	if KeyUsageAcceptorSign != 23 {
		t.Errorf("KeyUsageAcceptorSign = %d, want 23", KeyUsageAcceptorSign)
	}
	if KeyUsageInitiatorSign != 25 {
		t.Errorf("KeyUsageInitiatorSign = %d, want 25", KeyUsageInitiatorSign)
	}
}

func TestKRB5OID(t *testing.T) {
	expected := []int{1, 2, 840, 113554, 1, 2, 2}
	if len(KRB5OID) != len(expected) {
		t.Fatalf("KRB5OID length = %d, want %d", len(KRB5OID), len(expected))
	}
	for i, v := range KRB5OID {
		if v != expected[i] {
			t.Errorf("KRB5OID[%d] = %d, want %d", i, v, expected[i])
		}
	}
}
