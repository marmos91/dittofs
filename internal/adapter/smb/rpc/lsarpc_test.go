package rpc

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/pkg/auth/sid"
)

// newTestLSAHandler creates an LSARPCHandler with a deterministic mapper.
func newTestLSAHandler() *LSARPCHandler {
	return NewLSARPCHandler(sid.NewSIDMapper(0, 0, 0))
}

// buildTestBindRequest creates a bind request for the LSA interface.
func buildTestBindRequest(callID uint32) []byte {
	// Build a minimal bind PDU for the LSA interface
	buf := make([]byte, 72)

	// Header
	buf[0] = 5  // version major
	buf[1] = 0  // version minor
	buf[2] = PDUBind
	buf[3] = FlagFirstFrag | FlagLastFrag
	buf[4] = 0x10 // data rep: little endian
	binary.LittleEndian.PutUint16(buf[8:10], 72)  // frag length
	binary.LittleEndian.PutUint32(buf[12:16], callID) // call ID

	// Bind body
	binary.LittleEndian.PutUint16(buf[16:18], 4280)  // max xmit frag
	binary.LittleEndian.PutUint16(buf[18:20], 4280)  // max recv frag
	binary.LittleEndian.PutUint32(buf[20:24], 0)     // assoc group
	buf[24] = 1 // num contexts
	// padding (3 bytes at 25-27)

	// Context entry
	binary.LittleEndian.PutUint16(buf[28:30], 0) // context ID
	buf[30] = 1 // num transfer syntaxes
	// padding (1 byte at 31)

	// Abstract syntax: LSA UUID
	copy(buf[32:48], LSAInterfaceUUID[:])
	binary.LittleEndian.PutUint32(buf[48:52], 0) // version

	// Transfer syntax: NDR
	copy(buf[52:68], NDRTransferSyntaxUUID[:])
	binary.LittleEndian.PutUint32(buf[68:72], 2) // version

	return buf
}

// buildTestRequest creates an RPC request PDU.
func buildTestRequest(callID uint32, opnum uint16, stubData []byte) []byte {
	fragLen := HeaderSize + 8 + len(stubData)
	buf := make([]byte, fragLen)

	// Header
	buf[0] = 5
	buf[1] = 0
	buf[2] = PDURequest
	buf[3] = FlagFirstFrag | FlagLastFrag
	buf[4] = 0x10 // data rep
	binary.LittleEndian.PutUint16(buf[8:10], uint16(fragLen))
	binary.LittleEndian.PutUint32(buf[12:16], callID)

	// Request body
	binary.LittleEndian.PutUint32(buf[16:20], uint32(len(stubData))) // alloc hint
	binary.LittleEndian.PutUint16(buf[20:22], 0)                     // context ID
	binary.LittleEndian.PutUint16(buf[22:24], opnum)

	copy(buf[24:], stubData)

	return buf
}

func TestLSARPC_Bind(t *testing.T) {
	h := newTestLSAHandler()

	bindData := buildTestBindRequest(1)
	bindReq, err := ParseBindRequest(bindData)
	if err != nil {
		t.Fatalf("ParseBindRequest: %v", err)
	}

	response := h.HandleBind(bindReq)
	if len(response) < HeaderSize {
		t.Fatalf("Bind response too short: %d bytes", len(response))
	}

	// Verify it's a bind ack
	hdr, err := ParseHeader(response)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if hdr.PacketType != PDUBindAck {
		t.Errorf("Response type = %d, want %d (BindAck)", hdr.PacketType, PDUBindAck)
	}
	if hdr.CallID != 1 {
		t.Errorf("CallID = %d, want 1", hdr.CallID)
	}
}

func TestLSARPC_OpenPolicy2(t *testing.T) {
	h := newTestLSAHandler()

	// OpenPolicy2 stub data (minimal - just needs to be parseable)
	stubData := make([]byte, 48)
	reqData := buildTestRequest(2, OpLsarOpenPolicy2, stubData)

	req, err := ParseRequest(reqData)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}

	response := h.HandleRequest(req)
	if len(response) < HeaderSize+8 {
		t.Fatalf("Response too short: %d bytes", len(response))
	}

	// Verify it's a response PDU
	hdr, err := ParseHeader(response)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if hdr.PacketType != PDUResponse {
		t.Errorf("Response type = %d, want %d (Response)", hdr.PacketType, PDUResponse)
	}

	// Extract stub data: starts at offset 24
	if len(response) < 24+24 {
		t.Fatalf("Response stub data too short for OpenPolicy2 response")
	}

	// Status is at offset 20 within stub data (after 20-byte policy handle)
	stubStart := 24
	status := binary.LittleEndian.Uint32(response[stubStart+20 : stubStart+24])
	if status != statusSuccess {
		t.Errorf("OpenPolicy2 status = 0x%08x, want 0x%08x (success)", status, statusSuccess)
	}
}

func TestLSARPC_Close(t *testing.T) {
	h := newTestLSAHandler()

	// Close stub data: 20-byte policy handle
	stubData := make([]byte, 20)
	stubData[0] = 0x01 // non-zero handle
	reqData := buildTestRequest(3, OpLsarClose, stubData)

	req, err := ParseRequest(reqData)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}

	response := h.HandleRequest(req)
	if len(response) < HeaderSize+8 {
		t.Fatalf("Response too short: %d bytes", len(response))
	}

	hdr, err := ParseHeader(response)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if hdr.PacketType != PDUResponse {
		t.Errorf("Response type = %d, want %d (Response)", hdr.PacketType, PDUResponse)
	}

	// Verify success status
	stubStart := 24
	if len(response) >= stubStart+24 {
		status := binary.LittleEndian.Uint32(response[stubStart+20 : stubStart+24])
		if status != statusSuccess {
			t.Errorf("Close status = 0x%08x, want 0x%08x (success)", status, statusSuccess)
		}
	}
}

// buildLookupSids2StubData builds a minimal LookupSids2 request with the given SIDs.
func buildLookupSids2StubData(sids []*sid.SID) []byte {
	var buf bytes.Buffer

	// Policy handle (20 bytes)
	policyHandle := make([]byte, 20)
	policyHandle[0] = 0x01
	buf.Write(policyHandle)

	// SID array: Count
	appendUint32Buf(&buf, uint32(len(sids)))
	// Pointer to array
	appendUint32Buf(&buf, 0x00020000)
	// Conformant array max count
	appendUint32Buf(&buf, uint32(len(sids)))

	// SID pointers
	for range sids {
		appendUint32Buf(&buf, 0x00020004)
	}

	// SID data
	for _, s := range sids {
		// SubAuthorityCount as conformant max
		appendUint32Buf(&buf, uint32(s.SubAuthorityCount))
		// SID binary
		sid.EncodeSID(&buf, s)
	}

	return buf.Bytes()
}

func TestLSARPC_LookupSids2_WellKnown(t *testing.T) {
	h := newTestLSAHandler()

	// Look up "Everyone" SID (S-1-1-0)
	sids := []*sid.SID{sid.WellKnownEveryone}
	stubData := buildLookupSids2StubData(sids)
	reqData := buildTestRequest(4, OpLsarLookupSids2, stubData)

	req, err := ParseRequest(reqData)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}

	response := h.HandleRequest(req)
	if len(response) < HeaderSize+8 {
		t.Fatalf("Response too short: %d bytes", len(response))
	}

	hdr, err := ParseHeader(response)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if hdr.PacketType != PDUResponse {
		t.Errorf("Response type = %d, want %d (Response)", hdr.PacketType, PDUResponse)
	}

	// The response should contain a valid stub with the resolved name.
	// We verify the response is long enough and doesn't have a fault status.
	if hdr.PacketType == PDUFault {
		t.Fatal("Got fault response instead of success")
	}

	// Verify the response stub data is non-empty
	if len(response) <= 24 {
		t.Fatal("Response stub data is empty")
	}
}

func TestLSARPC_LookupSids2_DomainUser(t *testing.T) {
	mapper := sid.NewSIDMapper(0, 0, 0)
	h := NewLSARPCHandler(mapper)

	// Look up domain user SID for UID 1000
	userSID := mapper.UserSID(1000)
	sids := []*sid.SID{userSID}
	stubData := buildLookupSids2StubData(sids)
	reqData := buildTestRequest(5, OpLsarLookupSids2, stubData)

	req, err := ParseRequest(reqData)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}

	response := h.HandleRequest(req)
	if len(response) < HeaderSize+8 {
		t.Fatalf("Response too short: %d bytes", len(response))
	}

	hdr, err := ParseHeader(response)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if hdr.PacketType == PDUFault {
		t.Fatal("Got fault response instead of success")
	}
}

func TestLSARPC_LookupSids2_Unknown(t *testing.T) {
	h := newTestLSAHandler()

	// Look up an unknown SID
	unknownSID := sid.ParseSIDMust("S-1-99-42")
	sids := []*sid.SID{unknownSID}
	stubData := buildLookupSids2StubData(sids)
	reqData := buildTestRequest(6, OpLsarLookupSids2, stubData)

	req, err := ParseRequest(reqData)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}

	response := h.HandleRequest(req)
	if len(response) < HeaderSize+8 {
		t.Fatalf("Response too short: %d bytes", len(response))
	}

	hdr, err := ParseHeader(response)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if hdr.PacketType == PDUFault {
		t.Fatal("Got fault response instead of success")
	}

	// Verify status indicates some not mapped (since the SID is unknown)
	// Status is the last 4 bytes of stub data
	stubData2 := response[24:]
	if len(stubData2) >= 4 {
		status := binary.LittleEndian.Uint32(stubData2[len(stubData2)-4:])
		if status != statusSomeNotMapped && status != statusSuccess {
			t.Errorf("Status = 0x%08x, want STATUS_SOME_NOT_MAPPED (0x%08x) or success",
				status, statusSomeNotMapped)
		}
	}
}

func TestLSARPC_UnsupportedOpnum(t *testing.T) {
	h := newTestLSAHandler()

	stubData := make([]byte, 16)
	reqData := buildTestRequest(7, 99, stubData) // opnum 99 is unsupported

	req, err := ParseRequest(reqData)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}

	response := h.HandleRequest(req)
	if len(response) < HeaderSize {
		t.Fatalf("Response too short: %d bytes", len(response))
	}

	hdr, err := ParseHeader(response)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if hdr.PacketType != PDUFault {
		t.Errorf("Expected fault for unsupported opnum, got type %d", hdr.PacketType)
	}
}

func TestIsSupportedPipe_LSARPC(t *testing.T) {
	tests := []struct {
		name     string
		expected bool
	}{
		{"lsarpc", true},
		{"\\lsarpc", true},
		{"\\pipe\\lsarpc", true},
		{"srvsvc", true},
		{"\\srvsvc", true},
		{"\\pipe\\srvsvc", true},
		{"unknown", false},
		{"samr", false},
	}

	for _, tt := range tests {
		result := IsSupportedPipe(tt.name)
		if result != tt.expected {
			t.Errorf("IsSupportedPipe(%q) = %v, want %v", tt.name, result, tt.expected)
		}
	}
}

func TestPipeManager_CreateLSARPCPipe(t *testing.T) {
	pm := NewPipeManager()
	mapper := sid.NewSIDMapper(111, 222, 333)
	pm.SetSIDMapper(mapper)

	fileID := [16]byte{1, 2, 3, 4}
	pipe := pm.CreatePipe(fileID, "lsarpc")

	if pipe == nil {
		t.Fatal("CreatePipe returned nil")
	}
	if pipe.Name != "lsarpc" {
		t.Errorf("pipe.Name = %q, want lsarpc", pipe.Name)
	}

	// Verify the pipe has an LSARPCHandler
	_, ok := pipe.Handler.(*LSARPCHandler)
	if !ok {
		t.Errorf("pipe.Handler type = %T, want *LSARPCHandler", pipe.Handler)
	}

	// Verify the pipe can be retrieved
	retrieved := pm.GetPipe(fileID)
	if retrieved != pipe {
		t.Error("GetPipe returned different pipe")
	}
}

func TestPipeManager_CreateSRVSVCPipe(t *testing.T) {
	pm := NewPipeManager()
	pm.SetShares([]ShareInfo1{{Name: "test", Type: STYPE_DISKTREE}})

	fileID := [16]byte{5, 6, 7, 8}
	pipe := pm.CreatePipe(fileID, "srvsvc")

	if pipe == nil {
		t.Fatal("CreatePipe returned nil")
	}

	// Verify the pipe has an SRVSVCHandler
	_, ok := pipe.Handler.(*SRVSVCHandler)
	if !ok {
		t.Errorf("pipe.Handler type = %T, want *SRVSVCHandler", pipe.Handler)
	}
}

func TestResolveSID_WellKnown(t *testing.T) {
	h := newTestLSAHandler()

	tests := []struct {
		sid      *sid.SID
		wantName string
		wantType uint16
	}{
		{sid.WellKnownEveryone, "Everyone", SidTypeWellKnownGroup},
		{sid.WellKnownSystem, "SYSTEM", SidTypeWellKnownGroup},
		{sid.WellKnownAdministrators, "Administrators", SidTypeAlias},
		{sid.WellKnownAnonymous, "ANONYMOUS LOGON", SidTypeWellKnownGroup},
	}

	for _, tt := range tests {
		r := h.resolveSID(tt.sid)
		if r.name != tt.wantName {
			t.Errorf("resolveSID(%s).name = %q, want %q", sid.FormatSID(tt.sid), r.name, tt.wantName)
		}
		if r.sidType != tt.wantType {
			t.Errorf("resolveSID(%s).sidType = %d, want %d", sid.FormatSID(tt.sid), r.sidType, tt.wantType)
		}
	}
}

func TestResolveSID_DomainUser(t *testing.T) {
	mapper := sid.NewSIDMapper(0, 0, 0)
	h := NewLSARPCHandler(mapper)

	userSID := mapper.UserSID(42)
	r := h.resolveSID(userSID)

	if r.name != "unix_user:42" {
		t.Errorf("resolveSID(user 42).name = %q, want unix_user:42", r.name)
	}
	if r.sidType != SidTypeUser {
		t.Errorf("resolveSID(user 42).sidType = %d, want %d (User)", r.sidType, SidTypeUser)
	}
}

func TestResolveSID_DomainGroup(t *testing.T) {
	mapper := sid.NewSIDMapper(0, 0, 0)
	h := NewLSARPCHandler(mapper)

	groupSID := mapper.GroupSID(100)
	r := h.resolveSID(groupSID)

	if r.name != "unix_group:100" {
		t.Errorf("resolveSID(group 100).name = %q, want unix_group:100", r.name)
	}
	if r.sidType != SidTypeGroup {
		t.Errorf("resolveSID(group 100).sidType = %d, want %d (Group)", r.sidType, SidTypeGroup)
	}
}

func TestResolveSID_Unknown(t *testing.T) {
	h := newTestLSAHandler()

	unknownSID := sid.ParseSIDMust("S-1-99-42")
	r := h.resolveSID(unknownSID)

	if r.sidType != SidTypeUnknown {
		t.Errorf("resolveSID(unknown).sidType = %d, want %d (Unknown)", r.sidType, SidTypeUnknown)
	}
	// Name should be the SID string for unknown SIDs
	if r.name != "S-1-99-42" {
		t.Errorf("resolveSID(unknown).name = %q, want S-1-99-42", r.name)
	}
}
