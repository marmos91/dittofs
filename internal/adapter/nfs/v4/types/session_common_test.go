package types

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// ============================================================================
// SessionId4 Round-Trip Tests
// ============================================================================

func TestSessionId4_RoundTrip(t *testing.T) {
	original := SessionId4{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	// Fixed-size opaque: exactly 16 bytes, no length prefix
	if buf.Len() != NFS4_SESSIONID_SIZE {
		t.Errorf("encoded size = %d, want %d", buf.Len(), NFS4_SESSIONID_SIZE)
	}

	var decoded SessionId4
	if err := decoded.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if decoded != original {
		t.Errorf("decoded = %v, want %v", decoded, original)
	}
}

func TestSessionId4_ZeroValue(t *testing.T) {
	original := SessionId4{}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	var decoded SessionId4
	if err := decoded.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if decoded != original {
		t.Errorf("zero-value round-trip failed")
	}
}

func TestSessionId4_String(t *testing.T) {
	sid := SessionId4{0xab, 0xcd, 0xef, 0x01, 0x23, 0x45, 0x67, 0x89,
		0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77}
	s := sid.String()
	if s != "abcdef012345678900112233445566"+"77" {
		// Verify it's valid hex
		if len(s) != 32 {
			t.Errorf("String() length = %d, want 32", len(s))
		}
	}
}

// ============================================================================
// ClientOwner4 Round-Trip Tests
// ============================================================================

func TestClientOwner4_RoundTrip(t *testing.T) {
	original := ClientOwner4{
		Verifier: [8]byte{1, 2, 3, 4, 5, 6, 7, 8},
		OwnerID:  []byte("test-client-owner-12345"),
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	var decoded ClientOwner4
	if err := decoded.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if decoded.Verifier != original.Verifier {
		t.Errorf("Verifier = %v, want %v", decoded.Verifier, original.Verifier)
	}
	if !bytes.Equal(decoded.OwnerID, original.OwnerID) {
		t.Errorf("OwnerID = %v, want %v", decoded.OwnerID, original.OwnerID)
	}
}

func TestClientOwner4_EmptyOwnerID(t *testing.T) {
	original := ClientOwner4{
		Verifier: [8]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		OwnerID:  []byte{},
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	var decoded ClientOwner4
	if err := decoded.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if decoded.Verifier != original.Verifier {
		t.Errorf("Verifier mismatch")
	}
	if len(decoded.OwnerID) != 0 {
		t.Errorf("OwnerID length = %d, want 0", len(decoded.OwnerID))
	}
}

// ============================================================================
// ServerOwner4 Round-Trip Tests
// ============================================================================

func TestServerOwner4_RoundTrip(t *testing.T) {
	original := ServerOwner4{
		MinorID: 42,
		MajorID: []byte("dittofs-server-001"),
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	var decoded ServerOwner4
	if err := decoded.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if decoded.MinorID != original.MinorID {
		t.Errorf("MinorID = %d, want %d", decoded.MinorID, original.MinorID)
	}
	if !bytes.Equal(decoded.MajorID, original.MajorID) {
		t.Errorf("MajorID = %v, want %v", decoded.MajorID, original.MajorID)
	}
}

// ============================================================================
// NfsImplId4 Round-Trip Tests
// ============================================================================

func TestNfsImplId4_RoundTrip(t *testing.T) {
	original := NfsImplId4{
		Domain: "dittofs.example.com",
		Name:   "DittoFS",
		Date:   NFS4Time{Seconds: 1700000000, Nseconds: 123456789},
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	var decoded NfsImplId4
	if err := decoded.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if decoded.Domain != original.Domain {
		t.Errorf("Domain = %q, want %q", decoded.Domain, original.Domain)
	}
	if decoded.Name != original.Name {
		t.Errorf("Name = %q, want %q", decoded.Name, original.Name)
	}
	if decoded.Date.Seconds != original.Date.Seconds {
		t.Errorf("Date.Seconds = %d, want %d", decoded.Date.Seconds, original.Date.Seconds)
	}
	if decoded.Date.Nseconds != original.Date.Nseconds {
		t.Errorf("Date.Nseconds = %d, want %d", decoded.Date.Nseconds, original.Date.Nseconds)
	}
}

func TestNfsImplId4_NegativeTimestamp(t *testing.T) {
	// Negative seconds (pre-epoch) should work since Seconds is int64
	original := NfsImplId4{
		Domain: "test.com",
		Name:   "test",
		Date:   NFS4Time{Seconds: -1000, Nseconds: 0},
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	var decoded NfsImplId4
	if err := decoded.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if decoded.Date.Seconds != original.Date.Seconds {
		t.Errorf("Date.Seconds = %d, want %d", decoded.Date.Seconds, original.Date.Seconds)
	}
}

// ============================================================================
// ChannelAttrs Round-Trip Tests
// ============================================================================

func TestChannelAttrs_RoundTrip_NoRdma(t *testing.T) {
	original := ChannelAttrs{
		HeaderPadSize:         0,
		MaxRequestSize:        1049620,
		MaxResponseSize:       1049620,
		MaxResponseSizeCached: 8192,
		MaxOperations:         16,
		MaxRequests:           64,
		RdmaIrd:               nil, // no RDMA
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	var decoded ChannelAttrs
	if err := decoded.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if decoded.HeaderPadSize != original.HeaderPadSize {
		t.Errorf("HeaderPadSize = %d, want %d", decoded.HeaderPadSize, original.HeaderPadSize)
	}
	if decoded.MaxRequestSize != original.MaxRequestSize {
		t.Errorf("MaxRequestSize = %d, want %d", decoded.MaxRequestSize, original.MaxRequestSize)
	}
	if decoded.MaxResponseSize != original.MaxResponseSize {
		t.Errorf("MaxResponseSize = %d, want %d", decoded.MaxResponseSize, original.MaxResponseSize)
	}
	if decoded.MaxResponseSizeCached != original.MaxResponseSizeCached {
		t.Errorf("MaxResponseSizeCached = %d, want %d", decoded.MaxResponseSizeCached, original.MaxResponseSizeCached)
	}
	if decoded.MaxOperations != original.MaxOperations {
		t.Errorf("MaxOperations = %d, want %d", decoded.MaxOperations, original.MaxOperations)
	}
	if decoded.MaxRequests != original.MaxRequests {
		t.Errorf("MaxRequests = %d, want %d", decoded.MaxRequests, original.MaxRequests)
	}
	if len(decoded.RdmaIrd) != 0 {
		t.Errorf("RdmaIrd length = %d, want 0", len(decoded.RdmaIrd))
	}
}

func TestChannelAttrs_RoundTrip_WithRdma(t *testing.T) {
	original := ChannelAttrs{
		HeaderPadSize:         0,
		MaxRequestSize:        1049620,
		MaxResponseSize:       1049620,
		MaxResponseSizeCached: 8192,
		MaxOperations:         16,
		MaxRequests:           64,
		RdmaIrd:               []uint32{256}, // RDMA with IRD=256
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	var decoded ChannelAttrs
	if err := decoded.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if len(decoded.RdmaIrd) != 1 {
		t.Fatalf("RdmaIrd length = %d, want 1", len(decoded.RdmaIrd))
	}
	if decoded.RdmaIrd[0] != 256 {
		t.Errorf("RdmaIrd[0] = %d, want 256", decoded.RdmaIrd[0])
	}
}

// ============================================================================
// StateProtect4A Round-Trip Tests
// ============================================================================

func TestStateProtect4A_RoundTrip_None(t *testing.T) {
	original := StateProtect4A{How: SP4_NONE}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	// SP4_NONE should be just the discriminant (4 bytes)
	if buf.Len() != 4 {
		t.Errorf("encoded size = %d, want 4", buf.Len())
	}

	var decoded StateProtect4A
	if err := decoded.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if decoded.How != SP4_NONE {
		t.Errorf("How = %d, want %d (SP4_NONE)", decoded.How, SP4_NONE)
	}
}

func TestStateProtect4A_RoundTrip_MachCred(t *testing.T) {
	original := StateProtect4A{
		How:        SP4_MACH_CRED,
		MachOps:    Bitmap4{0x00000003, 0x00000001},
		EnforceOps: Bitmap4{0x00000001},
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	var decoded StateProtect4A
	if err := decoded.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if decoded.How != SP4_MACH_CRED {
		t.Errorf("How = %d, want %d (SP4_MACH_CRED)", decoded.How, SP4_MACH_CRED)
	}
	if len(decoded.MachOps) != 2 {
		t.Fatalf("MachOps length = %d, want 2", len(decoded.MachOps))
	}
	if decoded.MachOps[0] != 0x00000003 || decoded.MachOps[1] != 0x00000001 {
		t.Errorf("MachOps = %v, want [3 1]", decoded.MachOps)
	}
	if len(decoded.EnforceOps) != 1 || decoded.EnforceOps[0] != 0x00000001 {
		t.Errorf("EnforceOps = %v, want [1]", decoded.EnforceOps)
	}
}

// ============================================================================
// StateProtect4R Round-Trip Tests
// ============================================================================

func TestStateProtect4R_RoundTrip_None(t *testing.T) {
	original := StateProtect4R{How: SP4_NONE}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	var decoded StateProtect4R
	if err := decoded.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if decoded.How != SP4_NONE {
		t.Errorf("How = %d, want %d (SP4_NONE)", decoded.How, SP4_NONE)
	}
}

func TestStateProtect4R_RoundTrip_MachCred(t *testing.T) {
	original := StateProtect4R{
		How:        SP4_MACH_CRED,
		MachOps:    Bitmap4{0xdeadbeef},
		EnforceOps: Bitmap4{0xcafebabe},
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	var decoded StateProtect4R
	if err := decoded.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if decoded.How != SP4_MACH_CRED {
		t.Errorf("How = %d, want %d", decoded.How, SP4_MACH_CRED)
	}
	if len(decoded.MachOps) != 1 || decoded.MachOps[0] != 0xdeadbeef {
		t.Errorf("MachOps = %v, want [0xdeadbeef]", decoded.MachOps)
	}
	if len(decoded.EnforceOps) != 1 || decoded.EnforceOps[0] != 0xcafebabe {
		t.Errorf("EnforceOps = %v, want [0xcafebabe]", decoded.EnforceOps)
	}
}

// ============================================================================
// CallbackSecParms4 Round-Trip Tests
// ============================================================================

func TestCallbackSecParms4_RoundTrip_AuthNone(t *testing.T) {
	original := CallbackSecParms4{CbSecFlavor: 0} // AUTH_NONE

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	// AUTH_NONE is just the discriminant (4 bytes)
	if buf.Len() != 4 {
		t.Errorf("encoded size = %d, want 4", buf.Len())
	}

	var decoded CallbackSecParms4
	if err := decoded.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if decoded.CbSecFlavor != 0 {
		t.Errorf("CbSecFlavor = %d, want 0 (AUTH_NONE)", decoded.CbSecFlavor)
	}
}

func TestCallbackSecParms4_RoundTrip_AuthSys(t *testing.T) {
	original := CallbackSecParms4{
		CbSecFlavor: 1, // AUTH_SYS
		AuthSysParms: &AuthSysParms{
			Stamp:       12345,
			MachineName: "test",
			UID:         1000,
			GID:         1000,
			GIDs:        []uint32{4, 24},
		},
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	var decoded CallbackSecParms4
	if err := decoded.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if decoded.CbSecFlavor != 1 {
		t.Errorf("CbSecFlavor = %d, want 1 (AUTH_SYS)", decoded.CbSecFlavor)
	}
	if decoded.AuthSysParms == nil {
		t.Fatal("AuthSysParms is nil")
	}
	if decoded.AuthSysParms.UID != 1000 {
		t.Errorf("UID = %d, want 1000", decoded.AuthSysParms.UID)
	}
	if decoded.AuthSysParms.MachineName != "test" {
		t.Errorf("MachineName = %q, want %q", decoded.AuthSysParms.MachineName, "test")
	}
}

// ============================================================================
// Bitmap4 Round-Trip Tests
// ============================================================================

func TestBitmap4_RoundTrip_Empty(t *testing.T) {
	original := Bitmap4{}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	// Empty bitmap: just count=0 (4 bytes)
	if buf.Len() != 4 {
		t.Errorf("encoded size = %d, want 4", buf.Len())
	}

	var decoded Bitmap4
	if err := decoded.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if len(decoded) != 0 {
		t.Errorf("decoded length = %d, want 0", len(decoded))
	}
}

func TestBitmap4_RoundTrip_NonEmpty(t *testing.T) {
	original := Bitmap4{0x00000001, 0x00000002, 0xdeadbeef}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	var decoded Bitmap4
	if err := decoded.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if len(decoded) != len(original) {
		t.Fatalf("decoded length = %d, want %d", len(decoded), len(original))
	}
	for i := range original {
		if decoded[i] != original[i] {
			t.Errorf("decoded[%d] = 0x%08x, want 0x%08x", i, decoded[i], original[i])
		}
	}
}

// ============================================================================
// ReferringCallTriple Round-Trip Tests
// ============================================================================

func TestReferringCallTriple_RoundTrip(t *testing.T) {
	original := ReferringCallTriple{
		SessionID: SessionId4{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x01, 0x02,
			0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a},
		ReferringCalls: []ReferringCall4{
			{SequenceID: 1, SlotID: 0},
			{SequenceID: 5, SlotID: 3},
		},
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	var decoded ReferringCallTriple
	if err := decoded.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if decoded.SessionID != original.SessionID {
		t.Errorf("SessionID mismatch")
	}
	if len(decoded.ReferringCalls) != 2 {
		t.Fatalf("ReferringCalls length = %d, want 2", len(decoded.ReferringCalls))
	}
	if decoded.ReferringCalls[0].SequenceID != 1 || decoded.ReferringCalls[0].SlotID != 0 {
		t.Errorf("ReferringCalls[0] = %v, want {1, 0}", decoded.ReferringCalls[0])
	}
	if decoded.ReferringCalls[1].SequenceID != 5 || decoded.ReferringCalls[1].SlotID != 3 {
		t.Errorf("ReferringCalls[1] = %v, want {5, 3}", decoded.ReferringCalls[1])
	}
}

func TestReferringCallTriple_RoundTrip_NoCalls(t *testing.T) {
	original := ReferringCallTriple{
		SessionID:      SessionId4{0x01},
		ReferringCalls: nil,
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	var decoded ReferringCallTriple
	if err := decoded.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if len(decoded.ReferringCalls) != 0 {
		t.Errorf("ReferringCalls length = %d, want 0", len(decoded.ReferringCalls))
	}
}

// ============================================================================
// ReferringCall4 Round-Trip Tests
// ============================================================================

func TestReferringCall4_RoundTrip(t *testing.T) {
	original := ReferringCall4{SequenceID: 42, SlotID: 7}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	var decoded ReferringCall4
	if err := decoded.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if decoded.SequenceID != original.SequenceID {
		t.Errorf("SequenceID = %d, want %d", decoded.SequenceID, original.SequenceID)
	}
	if decoded.SlotID != original.SlotID {
		t.Errorf("SlotID = %d, want %d", decoded.SlotID, original.SlotID)
	}
}

// ============================================================================
// CallbackSecParms4 wire-format tests
// ============================================================================

// xdrOpaque appends a variable-length XDR opaque: 32-bit length, the bytes,
// then zero padding out to a 4-byte boundary.
func xdrOpaque(b []byte, v []byte) []byte {
	b = append(b, byte(len(v)>>24), byte(len(v)>>16), byte(len(v)>>8), byte(len(v)))
	b = append(b, v...)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

// xdrU32 appends a 32-bit big-endian value.
func xdrU32(b []byte, v uint32) []byte {
	return append(b, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

// TestCallbackSecParms4_DecodeWireFormat decodes a callback_sec_parms4 array
// built byte by byte from the XDR in RFC 8881 Section 18.35, rather than from
// this package's own Encode. An RPCSEC_GSS entry sits between an AUTH_NONE and
// an AUTH_SYS entry, so reading gss_cb_handles4 with the wrong shape consumes
// the wrong number of bytes and the AUTH_SYS entry behind it decodes as
// garbage or fails outright. A symmetric encode/decode round trip cannot
// detect that: it agrees with itself whatever shape it uses.
func TestCallbackSecParms4_DecodeWireFormat(t *testing.T) {
	var w []byte
	// [0] AUTH_NONE: discriminant only, void arm.
	w = xdrU32(w, 0)
	// [1] RPCSEC_GSS: gcbp_service, then two gsshandle4_t opaques.
	w = xdrU32(w, 6)
	w = xdrU32(w, 3) // RPC_GSS_SVC_PRIVACY
	w = xdrOpaque(w, []byte("Handle from server"))
	w = xdrOpaque(w, []byte("Client handle"))
	// [2] AUTH_SYS: authsys_parms fields inline (RFC 5531 Section 8.2).
	w = xdrU32(w, 1)
	w = xdrU32(w, 5) // stamp
	w = xdrOpaque(w, []byte("Random machine name"))
	w = xdrU32(w, 7)  // uid
	w = xdrU32(w, 11) // gid
	w = xdrU32(w, 0)  // gids<>, empty

	r := bytes.NewReader(w)
	got := make([]CallbackSecParms4, 3)
	for i := range got {
		if err := got[i].Decode(r); err != nil {
			t.Fatalf("entry[%d] decode failed: %v", i, err)
		}
	}
	if r.Len() != 0 {
		t.Errorf("%d bytes left unread; the array did not consume exactly what it was given", r.Len())
	}

	if got[0].CbSecFlavor != 0 {
		t.Errorf("entry[0] flavor = %d, want 0 (AUTH_NONE)", got[0].CbSecFlavor)
	}

	g := got[1].GssCbHandles
	if got[1].CbSecFlavor != 6 || g == nil {
		t.Fatalf("entry[1] = flavor %d, handles %+v; want flavor 6 with handles", got[1].CbSecFlavor, g)
	}
	if g.Service != 3 {
		t.Errorf("entry[1] gcbp_service = %d, want 3", g.Service)
	}
	if string(g.HandleFromServer) != "Handle from server" {
		t.Errorf("entry[1] gcbp_handle_from_server = %q, want %q", g.HandleFromServer, "Handle from server")
	}
	if string(g.HandleFromClient) != "Client handle" {
		t.Errorf("entry[1] gcbp_handle_from_client = %q, want %q", g.HandleFromClient, "Client handle")
	}

	// The AUTH_SYS entry is the alignment witness: it only decodes correctly
	// if the RPCSEC_GSS entry ahead of it consumed exactly its own bytes.
	p := got[2].AuthSysParms
	if got[2].CbSecFlavor != 1 || p == nil {
		t.Fatalf("entry[2] = flavor %d, parms %+v; want flavor 1 with parms", got[2].CbSecFlavor, p)
	}
	if p.Stamp != 5 || p.MachineName != "Random machine name" || p.UID != 7 || p.GID != 11 || len(p.GIDs) != 0 {
		t.Errorf("entry[2] authsys_parms = %+v, want stamp 5, machine %q, uid 7, gid 11, no gids",
			p, "Random machine name")
	}
}

// TestCallbackSecParms4_EncodeMatchesWireFormat pins Encode to the same byte
// layout Decode reads, so the two cannot drift back into agreeing with each
// other on a shape no client sends.
func TestCallbackSecParms4_EncodeMatchesWireFormat(t *testing.T) {
	var want []byte
	want = xdrU32(want, 6)
	want = xdrU32(want, 3)
	want = xdrOpaque(want, []byte("Handle from server"))
	want = xdrOpaque(want, []byte("Client handle"))

	c := CallbackSecParms4{
		CbSecFlavor: 6,
		GssCbHandles: &GssCbHandles4{
			Service:          3,
			HandleFromServer: []byte("Handle from server"),
			HandleFromClient: []byte("Client handle"),
		},
	}
	var buf bytes.Buffer
	if err := c.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("Encode = % x, want % x", buf.Bytes(), want)
	}
}

// ============================================================================
// RFC 5531 Section 8.2 bounds on the callback AUTH_SYS credential
// ============================================================================

// encodeAuthSysRaw builds the XDR for a callback_sec_parms4 with CbSecFlavor
// AUTH_SYS, taking the machinename length and gids count as raw knobs so a test
// can build a credential past either bound. The XDR encoder will not produce
// one, which is the point: the bound is on what the server accepts off the wire.
func encodeAuthSysRaw(t *testing.T, machineNameLen int, gidsCount uint32) []byte {
	t.Helper()
	var buf bytes.Buffer
	var b [4]byte
	put := func(v uint32) {
		binary.BigEndian.PutUint32(b[:], v)
		buf.Write(b[:])
	}
	put(1) // cb_secflavor = AUTH_SYS
	put(42) // stamp
	put(uint32(machineNameLen))
	buf.Write(bytes.Repeat([]byte{'a'}, machineNameLen))
	buf.Write(bytes.Repeat([]byte{0}, (4-machineNameLen%4)%4)) // pad to 4
	put(1000) // uid
	put(1000) // gid
	put(gidsCount)
	for i := uint32(0); i < gidsCount; i++ {
		put(i)
	}
	return buf.Bytes()
}

func TestCallbackSecParms4_AuthSysMachineNameBound(t *testing.T) {
	tests := []struct {
		name    string
		len     int
		wantErr bool
	}{
		{"empty is valid", 0, false},
		{"exactly 255 accepted", 255, false},
		{"256 rejected", 256, true},
		{"far past the bound rejected", 4096, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := encodeAuthSysRaw(t, tt.len, 0)
			var p CallbackSecParms4
			err := p.Decode(bytes.NewReader(raw))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Decode accepted a %d-byte machinename, want rejection", tt.len)
				}
				if !errors.Is(err, ErrCallbackAuthSysBounds) {
					t.Fatalf("err = %v, want ErrCallbackAuthSysBounds", err)
				}
				if p.AuthSysParms != nil {
					t.Error("AuthSysParms was stored despite a rejected credential")
				}
				return
			}
			if err != nil {
				t.Fatalf("Decode failed: %v", err)
			}
			if p.AuthSysParms == nil || len(p.AuthSysParms.MachineName) != tt.len {
				t.Fatalf("machinename length = %v, want %d", p.AuthSysParms, tt.len)
			}
		})
	}
}

func TestCallbackSecParms4_AuthSysGIDsBound(t *testing.T) {
	// The gids<16> bound predates the machinename one; this pins it so a later
	// edit to the decode cannot quietly drop it while adding another bound.
	tests := []struct {
		name    string
		count   uint32
		wantErr bool
	}{
		{"16 accepted", 16, false},
		{"17 rejected", 17, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := encodeAuthSysRaw(t, 4, tt.count)
			var p CallbackSecParms4
			err := p.Decode(bytes.NewReader(raw))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Decode accepted %d gids, want rejection", tt.count)
				}
				if !errors.Is(err, ErrCallbackAuthSysBounds) {
					t.Fatalf("err = %v, want ErrCallbackAuthSysBounds", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Decode failed: %v", err)
			}
			if got := len(p.AuthSysParms.GIDs); got != int(tt.count) {
				t.Fatalf("len(GIDs) = %d, want %d", got, tt.count)
			}
		})
	}
}
