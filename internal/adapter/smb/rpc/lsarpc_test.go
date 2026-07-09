package rpc

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/pkg/auth/sid"
)

// mockResolver is a test IdentityResolver that returns preset names.
type mockResolver struct {
	users  map[uint32]string
	groups map[uint32]string
}

func (m *mockResolver) LookupUsernameByUID(uid uint32) (string, bool) {
	name, ok := m.users[uid]
	return name, ok
}

func (m *mockResolver) LookupGroupNameByGID(gid uint32) (string, bool) {
	name, ok := m.groups[gid]
	return name, ok
}

// newTestLSAHandler creates an LSARPCHandler with a deterministic mapper and no resolver.
func newTestLSAHandler() *LSARPCHandler {
	return NewLSARPCHandler(sid.NewSIDMapper(0, 0, 0), nil)
}

// buildTestBindRequest creates a bind request for the LSA interface.
func buildTestBindRequest(callID uint32) []byte {
	// Build a minimal bind PDU for the LSA interface
	buf := make([]byte, 72)

	// Header
	buf[0] = 5 // version major
	buf[1] = 0 // version minor
	buf[2] = PDUBind
	buf[3] = FlagFirstFrag | FlagLastFrag
	buf[4] = 0x10                                     // data rep: little endian
	binary.LittleEndian.PutUint16(buf[8:10], 72)      // frag length
	binary.LittleEndian.PutUint32(buf[12:16], callID) // call ID

	// Bind body
	binary.LittleEndian.PutUint16(buf[16:18], 4280) // max xmit frag
	binary.LittleEndian.PutUint16(buf[18:20], 4280) // max recv frag
	binary.LittleEndian.PutUint32(buf[20:24], 0)    // assoc group
	buf[24] = 1                                     // num contexts
	// padding (3 bytes at 25-27)

	// Context entry
	binary.LittleEndian.PutUint16(buf[28:30], 0) // context ID
	buf[30] = 1                                  // num transfer syntaxes
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

// TestLSARPC_OpenPolicy verifies the legacy LsarOpenPolicy (opnum 6) returns a
// success policy handle, not a fault. smbcacls and older clients call opnum 6
// before LookupSids; faulting it makes them fall back to raw SIDs (#1291).
func TestLSARPC_OpenPolicy(t *testing.T) {
	h := newTestLSAHandler()

	// LsarOpenPolicy stub data (minimal - handler ignores it).
	stubData := make([]byte, 48)
	reqData := buildTestRequest(2, OpLsarOpenPolicy, stubData)

	req, err := ParseRequest(reqData)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}

	response := h.HandleRequest(req)

	hdr, err := ParseHeader(response)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if hdr.PacketType != PDUResponse {
		t.Fatalf("LsarOpenPolicy (opnum 6) returned PDU type %d, want %d (Response) — clients fall back to raw SIDs on a fault", hdr.PacketType, PDUResponse)
	}

	// Status is at offset 20 within stub data (after 20-byte policy handle).
	if len(response) < 24+24 {
		t.Fatalf("Response stub data too short for OpenPolicy response")
	}
	stubStart := 24
	status := binary.LittleEndian.Uint32(response[stubStart+20 : stubStart+24])
	if status != statusSuccess {
		t.Errorf("LsarOpenPolicy status = 0x%08x, want 0x%08x (success)", status, statusSuccess)
	}
}

// TestLSARPC_OpenPolicy3 verifies LsarOpenPolicy3 (opnum 130) returns a success
// policy handle in the OpenPolicy3 layout, not a fault. Windows 10/11 Explorer's
// Security tab (aclui.dll) calls opnum 130 FIRST; faulting it makes the ACL editor
// show raw SIDs and can make the file Properties dialog fail to open for an
// AD-owned file (#1343). Verified live: real Windows then resolves DITTOFS\alice.
func TestLSARPC_OpenPolicy3(t *testing.T) {
	h := newTestLSAHandler()

	// LsarOpenPolicy3 request stub (handler ignores its contents).
	reqData := buildTestRequest(2, OpLsarOpenPolicy3, make([]byte, 48))
	req, err := ParseRequest(reqData)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}

	response := h.HandleRequest(req)
	hdr, err := ParseHeader(response)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if hdr.PacketType != PDUResponse {
		t.Fatalf("LsarOpenPolicy3 (opnum 130) returned PDU type %d, want %d (Response) — Windows shows raw SIDs / Properties fails on a fault", hdr.PacketType, PDUResponse)
	}

	// OpenPolicy3 layout: OutVersion(4) + RevisionInfo{disc(4),Revision(4),
	// SupportedFeatures(4)} + PolicyHandle(20) + status(4) = 40-byte stub.
	stub := response[24:]
	if len(stub) < 40 {
		t.Fatalf("OpenPolicy3 stub = %d bytes, want >= 40 (out-params precede the handle)", len(stub))
	}
	if v := binary.LittleEndian.Uint32(stub[0:4]); v != 1 {
		t.Errorf("OutVersion = %d, want 1", v)
	}
	// Handle must be non-zero so the client can use it for the follow-up LookupSids.
	handle := stub[16:36]
	allZero := true
	for _, b := range handle {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		t.Error("OpenPolicy3 returned a zero policy handle")
	}
	if status := binary.LittleEndian.Uint32(stub[36:40]); status != statusSuccess {
		t.Errorf("OpenPolicy3 status = 0x%08x, want 0x%08x (success)", status, statusSuccess)
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
	h := NewLSARPCHandler(mapper, nil)

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

func TestResolveSID_DomainUser_NoResolver(t *testing.T) {
	mapper := sid.NewSIDMapper(0, 0, 0)
	h := NewLSARPCHandler(mapper, nil)

	userSID := mapper.UserSID(42)
	r := h.resolveSID(userSID)

	if r.name != "unix_user:42" {
		t.Errorf("resolveSID(user 42).name = %q, want unix_user:42", r.name)
	}
	if r.sidType != SidTypeUser {
		t.Errorf("resolveSID(user 42).sidType = %d, want %d (User)", r.sidType, SidTypeUser)
	}
	if r.domainName != "DITTOFS" {
		t.Errorf("resolveSID(user 42).domainName = %q, want DITTOFS", r.domainName)
	}
}

func TestResolveSID_DomainUser_WithResolver(t *testing.T) {
	mapper := sid.NewSIDMapper(0, 0, 0)
	resolver := &mockResolver{
		users: map[uint32]string{42: "alice"},
	}
	h := NewLSARPCHandler(mapper, resolver)

	userSID := mapper.UserSID(42)
	r := h.resolveSID(userSID)

	if r.name != "alice" {
		t.Errorf("resolveSID(user 42).name = %q, want alice", r.name)
	}
	if r.sidType != SidTypeUser {
		t.Errorf("resolveSID(user 42).sidType = %d, want %d (User)", r.sidType, SidTypeUser)
	}
	if r.domainName != "DITTOFS" {
		t.Errorf("resolveSID(user 42).domainName = %q, want DITTOFS", r.domainName)
	}
}

func TestResolveSID_DomainGroup_NoResolver(t *testing.T) {
	mapper := sid.NewSIDMapper(0, 0, 0)
	h := NewLSARPCHandler(mapper, nil)

	groupSID := mapper.GroupSID(100)
	r := h.resolveSID(groupSID)

	if r.name != "unix_group:100" {
		t.Errorf("resolveSID(group 100).name = %q, want unix_group:100", r.name)
	}
	if r.sidType != SidTypeGroup {
		t.Errorf("resolveSID(group 100).sidType = %d, want %d (Group)", r.sidType, SidTypeGroup)
	}
	if r.domainName != "DITTOFS" {
		t.Errorf("resolveSID(group 100).domainName = %q, want DITTOFS", r.domainName)
	}
}

func TestResolveSID_DomainGroup_WithResolver(t *testing.T) {
	mapper := sid.NewSIDMapper(0, 0, 0)
	resolver := &mockResolver{
		groups: map[uint32]string{100: "editors"},
	}
	h := NewLSARPCHandler(mapper, resolver)

	groupSID := mapper.GroupSID(100)
	r := h.resolveSID(groupSID)

	if r.name != "editors" {
		t.Errorf("resolveSID(group 100).name = %q, want editors", r.name)
	}
	if r.sidType != SidTypeGroup {
		t.Errorf("resolveSID(group 100).sidType = %d, want %d (Group)", r.sidType, SidTypeGroup)
	}
	if r.domainName != "DITTOFS" {
		t.Errorf("resolveSID(group 100).domainName = %q, want DITTOFS", r.domainName)
	}
}

func TestResolveSID_DomainUser_ResolverMiss(t *testing.T) {
	mapper := sid.NewSIDMapper(0, 0, 0)
	// Resolver has no entry for UID 42 — should fallback to generic name
	resolver := &mockResolver{
		users: map[uint32]string{999: "bob"},
	}
	h := NewLSARPCHandler(mapper, resolver)

	userSID := mapper.UserSID(42)
	r := h.resolveSID(userSID)

	if r.name != "unix_user:42" {
		t.Errorf("resolveSID(user 42).name = %q, want unix_user:42", r.name)
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

func TestPipeManager_IdentityResolver(t *testing.T) {
	pm := NewPipeManager()
	mapper := sid.NewSIDMapper(0, 0, 0)
	pm.SetSIDMapper(mapper)

	resolver := &mockResolver{
		users:  map[uint32]string{1000: "alice"},
		groups: map[uint32]string{1000: "users"},
	}
	pm.SetIdentityResolver(resolver)

	fileID := [16]byte{10, 11, 12}
	pipe := pm.CreatePipe(fileID, "lsarpc")
	if pipe == nil {
		t.Fatal("CreatePipe returned nil")
	}

	lsaHandler, ok := pipe.Handler.(*LSARPCHandler)
	if !ok {
		t.Fatalf("pipe.Handler type = %T, want *LSARPCHandler", pipe.Handler)
	}

	// Verify resolver is wired through
	if lsaHandler.resolver == nil {
		t.Fatal("LSARPCHandler.resolver is nil, expected resolver to be set")
	}

	// Verify resolver works end-to-end
	userSID := mapper.UserSID(1000)
	r := lsaHandler.resolveSID(userSID)
	if r.name != "alice" {
		t.Errorf("resolveSID(user 1000).name = %q, want alice", r.name)
	}
}

func TestStoreIdentityResolver(t *testing.T) {
	r := &StoreIdentityResolver{
		LookupUser: func(uid uint32) (string, bool) {
			if uid == 1000 {
				return "alice", true
			}
			return "", false
		},
		LookupGroup: func(gid uint32) (string, bool) {
			if gid == 1000 {
				return "users", true
			}
			return "", false
		},
	}

	if name, ok := r.LookupUsernameByUID(1000); !ok || name != "alice" {
		t.Errorf("LookupUsernameByUID(1000) = (%q, %v), want (alice, true)", name, ok)
	}
	if _, ok := r.LookupUsernameByUID(9999); ok {
		t.Error("LookupUsernameByUID(9999) should return false")
	}
	if name, ok := r.LookupGroupNameByGID(1000); !ok || name != "users" {
		t.Errorf("LookupGroupNameByGID(1000) = (%q, %v), want (users, true)", name, ok)
	}
	if _, ok := r.LookupGroupNameByGID(9999); ok {
		t.Error("LookupGroupNameByGID(9999) should return false")
	}
}

func TestStoreIdentityResolver_NilFuncs(t *testing.T) {
	r := &StoreIdentityResolver{}

	if _, ok := r.LookupUsernameByUID(1000); ok {
		t.Error("LookupUsernameByUID should return false when LookupUser is nil")
	}
	if _, ok := r.LookupGroupNameByGID(1000); ok {
		t.Error("LookupGroupNameByGID should return false when LookupGroup is nil")
	}
}

// =============================================================================
// Foreign (AD/LDAP) SID resolution
// =============================================================================

// mockForeignResolver is a test ForeignSIDResolver. SID hits are keyed by
// canonical SID string; reverse uid/gid hits (the AD-only owner path) are keyed
// by numeric id.
type mockForeignResolver struct {
	hits    map[string]foreignHit
	uidHits map[uint32]reverseHit
	gidHits map[uint32]reverseHit
}

type foreignHit struct {
	name    string
	domain  string
	sidType uint16
}

type reverseHit struct {
	name   string
	domain string
}

func (m *mockForeignResolver) LookupSID(sidString string) (string, string, uint16, bool) {
	h, ok := m.hits[sidString]
	if !ok {
		return "", "", 0, false
	}
	return h.name, h.domain, h.sidType, true
}

func (m *mockForeignResolver) LookupUID(uid uint32) (string, string, bool) {
	h, ok := m.uidHits[uid]
	if !ok {
		return "", "", false
	}
	return h.name, h.domain, true
}

func (m *mockForeignResolver) LookupGID(gid uint32) (string, string, bool) {
	h, ok := m.gidHits[gid]
	if !ok {
		return "", "", false
	}
	return h.name, h.domain, true
}

func TestResolveSID_ForeignAD_Hit(t *testing.T) {
	mapper := sid.NewSIDMapper(0, 0, 0)
	adSID := sid.ParseSIDMust("S-1-5-21-1111111111-2222222222-3333333333-1107")
	resolver := &mockForeignResolver{
		hits: map[string]foreignHit{
			sid.FormatSID(adSID): {name: "alice", domain: "CONTOSO", sidType: SidTypeUser},
		},
	}
	h := NewLSARPCHandler(mapper, nil)
	h.SetForeignSIDResolver(resolver)

	r := h.resolveSID(adSID)
	if r.name != "alice" {
		t.Errorf("name = %q, want alice", r.name)
	}
	if r.domainName != "CONTOSO" {
		t.Errorf("domainName = %q, want CONTOSO", r.domainName)
	}
	if r.sidType != SidTypeUser {
		t.Errorf("sidType = %d, want %d (User)", r.sidType, SidTypeUser)
	}
	// The referenced domain SID must be the account SID with its RID stripped.
	wantDomSID := sid.FormatSID(sid.ParseSIDMust("S-1-5-21-1111111111-2222222222-3333333333"))
	if r.domainSID == nil || sid.FormatSID(r.domainSID) != wantDomSID {
		t.Errorf("domainSID = %v, want %s", r.domainSID, wantDomSID)
	}
}

func TestResolveSID_ForeignAD_Miss(t *testing.T) {
	mapper := sid.NewSIDMapper(0, 0, 0)
	adSID := sid.ParseSIDMust("S-1-5-21-1111111111-2222222222-3333333333-1107")
	resolver := &mockForeignResolver{hits: map[string]foreignHit{}} // empty: always misses
	h := NewLSARPCHandler(mapper, nil)
	h.SetForeignSIDResolver(resolver)

	r := h.resolveSID(adSID)
	if r.sidType != SidTypeUnknown {
		t.Errorf("miss sidType = %d, want %d (Unknown)", r.sidType, SidTypeUnknown)
	}
	if r.name != sid.FormatSID(adSID) {
		t.Errorf("miss name = %q, want raw SID %q", r.name, sid.FormatSID(adSID))
	}
}

func TestResolveSID_ForeignAD_NilResolver(t *testing.T) {
	// With no foreign resolver wired, a foreign SID is unknown (raw), never a panic.
	h := newTestLSAHandler()
	adSID := sid.ParseSIDMust("S-1-5-21-1111111111-2222222222-3333333333-1107")
	r := h.resolveSID(adSID)
	if r.sidType != SidTypeUnknown {
		t.Errorf("sidType = %d, want %d (Unknown)", r.sidType, SidTypeUnknown)
	}
}

func TestResolveSID_MachineDomainName_Configurable(t *testing.T) {
	mapper := sid.NewSIDMapper(0, 0, 0)
	h := NewLSARPCHandler(mapper, &mockResolver{users: map[uint32]string{7: "bob"}})

	// Default machine domain name.
	if got := h.resolveSID(mapper.UserSID(7)).domainName; got != "DITTOFS" {
		t.Errorf("default domainName = %q, want DITTOFS", got)
	}

	// Overridden machine domain name applies to machine-domain SIDs only.
	h.SetMachineDomainName("FILER01")
	if got := h.resolveSID(mapper.UserSID(7)).domainName; got != "FILER01" {
		t.Errorf("overridden domainName = %q, want FILER01", got)
	}

	// Resetting with empty restores the default.
	h.SetMachineDomainName("")
	if got := h.resolveSID(mapper.UserSID(7)).domainName; got != "DITTOFS" {
		t.Errorf("reset domainName = %q, want DITTOFS", got)
	}
}

// lookupSidsResult is a decoded LsarLookupSids response (the parts under test).
type lookupSidsResult struct {
	domains     []string // referenced-domain names, in list order
	names       []translatedName
	mappedCount uint32
	status      uint32
}

type translatedName struct {
	sidType   uint16
	domainIdx int32
}

// parseLookupSidsResponse decodes the stub data produced by
// buildLookupSidsResponse. It mirrors that exact layout — it is a test-only
// reader, not a general NDR parser. withFlags selects the EX (opnum 57/76)
// vs non-EX (opnum 15) translated-name entry layout.
func parseLookupSidsResponse(t *testing.T, response []byte, withFlags bool) lookupSidsResult {
	t.Helper()
	if len(response) <= 24 {
		t.Fatalf("response too short: %d bytes", len(response))
	}
	stub := response[24:]
	off := 0
	u32 := func() uint32 {
		v := binary.LittleEndian.Uint32(stub[off : off+4])
		off += 4
		return v
	}
	u16 := func() uint16 {
		v := binary.LittleEndian.Uint16(stub[off : off+2])
		off += 2
		return v
	}

	var res lookupSidsResult

	// --- Referenced domain list ---
	_ = u32() // list pointer
	domCount := u32()
	_ = u32() // array pointer
	_ = u32() // max entries

	type domFixed struct{ nameUTF16Bytes uint16 }
	var domFixeds []domFixed
	if domCount > 0 {
		_ = u32() // conformant max count
		for range domCount {
			length := u16() // Length (excl null), in bytes
			_ = u16()       // MaximumLength
			_ = u32()       // string pointer
			_ = u32()       // SID pointer
			domFixeds = append(domFixeds, domFixed{nameUTF16Bytes: length})
		}
		// Deferred domain pointees in NDR appearance order: each entry's Name
		// buffer THEN its SID (interleaved), not all-names-then-all-SIDs.
		for _, d := range domFixeds {
			name := readNDRUnicodeString(t, stub, &off)
			_ = d
			res.domains = append(res.domains, name)
			skipNDRSID(t, stub, &off)
		}
	}

	// --- Translated names ---
	nameCount := u32()
	_ = u32() // array pointer
	if nameCount > 0 {
		_ = u32() // conformant max count
		for range nameCount {
			st := u16()
			_ = u16() // pad/reserved
			_ = u16() // Length
			_ = u16() // MaximumLength
			_ = u32() // name pointer
			domIdx := int32(u32())
			if withFlags {
				_ = u32() // flags (EX layout only)
			}
			res.names = append(res.names, translatedName{sidType: st, domainIdx: domIdx})
		}
		// Deferred name strings.
		for range nameCount {
			readNDRUnicodeString(t, stub, &off)
		}
	}

	res.mappedCount = u32()
	res.status = u32()
	return res
}

// readNDRUnicodeString decodes a writeNDRUnicodeString-encoded string and
// advances off past the 4-byte padding. The buffer is an un-terminated
// conformant+varying array: MaxCount == ActualCount == char_count (no NUL).
func readNDRUnicodeString(t *testing.T, b []byte, off *int) string {
	t.Helper()
	maxCount := binary.LittleEndian.Uint32(b[*off : *off+4])
	*off += 4
	*off += 4 // offset
	actual := binary.LittleEndian.Uint32(b[*off : *off+4])
	*off += 4
	// MaxCount and ActualCount must agree and carry NO null terminator.
	if maxCount != actual {
		t.Fatalf("NDR string MaxCount=%d != ActualCount=%d", maxCount, actual)
	}
	chars := int(actual)
	byteLen := chars * 2
	data := b[*off : *off+byteLen]
	*off += byteLen
	// 4-byte align.
	for *off%4 != 0 {
		*off++
	}
	// Decode UTF-16LE; there is no trailing NUL to drop.
	var sb []rune
	for i := 0; i+1 < len(data); i += 2 {
		c := binary.LittleEndian.Uint16(data[i : i+2])
		sb = append(sb, rune(c))
	}
	return string(sb)
}

// skipNDRSID advances off past a writeNDRSID-encoded SID.
func skipNDRSID(t *testing.T, b []byte, off *int) {
	t.Helper()
	_ = binary.LittleEndian.Uint32(b[*off : *off+4]) // conformant max count
	*off += 4
	// SID: revision(1) + subauthcount(1) + authority(6) + subauths(4*n)
	subCount := int(b[*off+1])
	*off += 8 + 4*subCount
	for *off%4 != 0 {
		*off++
	}
}

// TestLookupSids_MixedBatch_MultiDomain exercises a batch containing a
// well-known SID, a machine-domain user, a foreign AD hit, and a foreign AD
// miss. It asserts STATUS_SOME_NOT_MAPPED, the correct MappedCount, per-SID
// types, and that the referenced-domain list correctly carries BUILTIN/the
// machine domain/the AD domain with the per-name domain index pointing at the
// right entry.
func TestLookupSids_MixedBatch_MultiDomain(t *testing.T) {
	mapper := sid.NewSIDMapper(0, 0, 0)
	localResolver := &mockResolver{users: map[uint32]string{5: "localuser"}}

	adHit := sid.ParseSIDMust("S-1-5-21-1111111111-2222222222-3333333333-1107")
	adMiss := sid.ParseSIDMust("S-1-5-21-999999999-888888888-777777777-1500")
	foreign := &mockForeignResolver{
		hits: map[string]foreignHit{
			sid.FormatSID(adHit): {name: "alice", domain: "CONTOSO", sidType: SidTypeUser},
		},
	}

	h := NewLSARPCHandler(mapper, localResolver)
	h.SetForeignSIDResolver(foreign)
	h.SetMachineDomainName("FILER01")

	sids := []*sid.SID{
		sid.WellKnownAdministrators, // BUILTIN\Administrators (alias)
		mapper.UserSID(5),           // FILER01\localuser
		adHit,                       // CONTOSO\alice
		adMiss,                      // unmapped
	}

	stubData := buildLookupSids2StubData(sids)
	reqData := buildTestRequest(42, OpLsarLookupSids2, stubData)
	req, err := ParseRequest(reqData)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	response := h.HandleRequest(req)

	hdr, err := ParseHeader(response)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if hdr.PacketType == PDUFault {
		t.Fatal("got fault for a partially-unmapped batch; must be a normal response")
	}

	res := parseLookupSidsResponse(t, response, true) // opnum 57 → EX layout

	if res.status != statusSomeNotMapped {
		t.Errorf("status = 0x%08x, want STATUS_SOME_NOT_MAPPED (0x%08x)", res.status, statusSomeNotMapped)
	}
	if res.mappedCount != 3 {
		t.Errorf("mappedCount = %d, want 3", res.mappedCount)
	}
	if len(res.names) != 4 {
		t.Fatalf("translated names = %d, want 4", len(res.names))
	}

	// Per-SID types.
	wantTypes := []uint16{SidTypeAlias, SidTypeUser, SidTypeUser, SidTypeUnknown}
	for i, wt := range wantTypes {
		if res.names[i].sidType != wt {
			t.Errorf("name[%d].sidType = %d, want %d", i, res.names[i].sidType, wt)
		}
	}

	// The unmapped entry must carry domain index -1.
	if res.names[3].domainIdx != -1 {
		t.Errorf("unmapped name domainIdx = %d, want -1", res.names[3].domainIdx)
	}

	// Referenced domains: BUILTIN, FILER01, CONTOSO (in first-seen order).
	wantDomains := []string{"BUILTIN", "FILER01", "CONTOSO"}
	if len(res.domains) != len(wantDomains) {
		t.Fatalf("referenced domains = %v, want %v", res.domains, wantDomains)
	}
	for i, wd := range wantDomains {
		if res.domains[i] != wd {
			t.Errorf("domain[%d] = %q, want %q", i, res.domains[i], wd)
		}
	}

	// Each mapped name's domain index must point at the matching domain entry.
	checkIdx := func(nameIdx int, wantDomain string) {
		di := res.names[nameIdx].domainIdx
		if di < 0 || int(di) >= len(res.domains) {
			t.Errorf("name[%d] domainIdx %d out of range", nameIdx, di)
			return
		}
		if res.domains[di] != wantDomain {
			t.Errorf("name[%d] -> domain %q, want %q", nameIdx, res.domains[di], wantDomain)
		}
	}
	checkIdx(0, "BUILTIN")
	checkIdx(1, "FILER01")
	checkIdx(2, "CONTOSO")
}

// TestBuildLookupSidsResponse_TwoDomains is a focused round-trip over the
// referenced-domain list with exactly two distinct domains, verifying the
// dedup/index bookkeeping in isolation from the request parser.
func TestBuildLookupSidsResponse_TwoDomains(t *testing.T) {
	h := newTestLSAHandler()

	resolved := []resolvedSID{
		{name: "alice", sidType: SidTypeUser, domainName: "CONTOSO", domainSID: sid.ParseSIDMust("S-1-5-21-1-2-3")},
		{name: "bob", sidType: SidTypeUser, domainName: "FABRIKAM", domainSID: sid.ParseSIDMust("S-1-5-21-4-5-6")},
		{name: "carol", sidType: SidTypeUser, domainName: "CONTOSO", domainSID: sid.ParseSIDMust("S-1-5-21-1-2-3")},
	}

	domainMap := make(map[string]int)
	var domains []domainEntry
	for i := range resolved {
		r := &resolved[i]
		if _, ok := domainMap[domainKeyOf(r)]; !ok {
			domainMap[domainKeyOf(r)] = len(domains)
			domains = append(domains, domainEntry{name: r.domainName, sid: r.domainSID})
		}
	}

	stub := h.buildLookupSidsResponse(resolved, domains, domainMap, true, true)
	// Prefix a fake 24-byte RPC header so the shared parser offset math holds.
	response := append(make([]byte, 24), stub...)
	res := parseLookupSidsResponse(t, response, true)

	if len(res.domains) != 2 {
		t.Fatalf("domains = %v, want exactly 2 (deduped)", res.domains)
	}
	if res.domains[0] != "CONTOSO" || res.domains[1] != "FABRIKAM" {
		t.Errorf("domains = %v, want [CONTOSO FABRIKAM]", res.domains)
	}
	if res.names[0].domainIdx != 0 || res.names[1].domainIdx != 1 || res.names[2].domainIdx != 0 {
		t.Errorf("domain indices = [%d %d %d], want [0 1 0]",
			res.names[0].domainIdx, res.names[1].domainIdx, res.names[2].domainIdx)
	}
	if res.mappedCount != 3 {
		t.Errorf("mappedCount = %d, want 3", res.mappedCount)
	}
	if res.status != statusSuccess {
		t.Errorf("status = 0x%08x, want success", res.status)
	}
}

// TestBuildLookupSidsResponse_SameNameDifferentSID reproduces the crash where an
// AD-owned file's owner (algorithmic MACHINE-domain SID, resolved to the AD
// account name + AD NetBIOS domain) shares a NetBIOS name with an AD SID grant
// (real AD domain SID). Deduping the referenced-domain list by NAME collapsed
// them, leaving the grant's SID inconsistent with the referenced domain SID —
// the client rejected the batch (raw SIDs) and Explorer crashed. The two domains
// must stay distinct so each account's RID pairs with the correct domain SID.
func TestBuildLookupSidsResponse_SameNameDifferentSID(t *testing.T) {
	h := newTestLSAHandler()
	machineDom := sid.ParseSIDMust("S-1-5-21-9-9-9")
	adDom := sid.ParseSIDMust("S-1-5-21-1-2-3")
	resolved := []resolvedSID{
		{name: "Administrator", sidType: SidTypeUser, domainName: "CUBBIT", domainSID: machineDom}, // owner, machine SID
		{name: "alice", sidType: SidTypeUser, domainName: "CUBBIT", domainSID: adDom},              // AD grant, AD SID
	}

	domainMap := make(map[string]int)
	var domains []domainEntry
	for i := range resolved {
		r := &resolved[i]
		if _, ok := domainMap[domainKeyOf(r)]; !ok {
			domainMap[domainKeyOf(r)] = len(domains)
			domains = append(domains, domainEntry{name: r.domainName, sid: r.domainSID})
		}
	}
	if len(domains) != 2 {
		t.Fatalf("domains = %d, want 2 (same name, different SID must NOT collapse)", len(domains))
	}

	stub := h.buildLookupSidsResponse(resolved, domains, domainMap, true, true)
	response := append(make([]byte, 24), stub...)
	res := parseLookupSidsResponse(t, response, true)

	if res.names[0].domainIdx == res.names[1].domainIdx {
		t.Errorf("owner and grant share domain index %d; must differ (distinct SIDs)", res.names[0].domainIdx)
	}
	if res.names[0].domainIdx < 0 || res.names[1].domainIdx < 0 {
		t.Fatalf("unmapped domain index: %d, %d", res.names[0].domainIdx, res.names[1].domainIdx)
	}
	if res.domains[res.names[0].domainIdx] != "CUBBIT" || res.domains[res.names[1].domainIdx] != "CUBBIT" {
		t.Errorf("both entries should display as CUBBIT; got %q, %q",
			res.domains[res.names[0].domainIdx], res.domains[res.names[1].domainIdx])
	}
}

// =============================================================================
// Machine-domain SID → directory reverse uid/gid (GAP 1: AD-only owner)
// =============================================================================

// TestResolveSID_MachineDomainUser_DirectoryFallback covers the headline #1343
// case: the file OWNER SID is the machine-domain (algorithmic) SID for an
// AD-only user (alice, uid 10001), so it decodes via UIDFromSID. The local
// control-plane resolver has no such user, so resolution must fall back to the
// directory by uidNumber and present the real AD name + domain.
func TestResolveSID_MachineDomainUser_DirectoryFallback(t *testing.T) {
	mapper := sid.NewSIDMapper(0, 0, 0)
	// Local resolver: deliberately empty for uid 10001 (no local account).
	local := &mockResolver{users: map[uint32]string{}}
	foreign := &mockForeignResolver{
		uidHits: map[uint32]reverseHit{10001: {name: "alice", domain: "DITTOFS"}},
	}
	h := NewLSARPCHandler(mapper, local)
	h.SetForeignSIDResolver(foreign)
	h.SetMachineDomainName("FILER01")

	ownerSID := mapper.UserSID(10001) // S-1-5-21-0-0-0-(10001*2+1000)
	r := h.resolveSID(ownerSID)

	if r.name != "alice" {
		t.Errorf("name = %q, want alice (directory reverse-uid)", r.name)
	}
	if r.sidType != SidTypeUser {
		t.Errorf("sidType = %d, want %d (User)", r.sidType, SidTypeUser)
	}
	// The directory-resolved domain (from the reverse lookup) takes precedence
	// over the machine NetBIOS name for an AD-only account.
	if r.domainName != "DITTOFS" {
		t.Errorf("domainName = %q, want DITTOFS (from directory)", r.domainName)
	}
	if r.domainSID == nil {
		t.Error("domainSID is nil; want the machine domain SID")
	}
}

// TestResolveSID_MachineDomainUser_LocalWins ensures the local store still wins
// when the user DOES have a local account — the directory fallback only fires
// on a local miss, and the machine NetBIOS name is used for local accounts.
func TestResolveSID_MachineDomainUser_LocalWins(t *testing.T) {
	mapper := sid.NewSIDMapper(0, 0, 0)
	local := &mockResolver{users: map[uint32]string{42: "localbob"}}
	foreign := &mockForeignResolver{
		uidHits: map[uint32]reverseHit{42: {name: "WRONG", domain: "WRONGDOM"}},
	}
	h := NewLSARPCHandler(mapper, local)
	h.SetForeignSIDResolver(foreign)
	h.SetMachineDomainName("FILER01")

	r := h.resolveSID(mapper.UserSID(42))
	if r.name != "localbob" {
		t.Errorf("name = %q, want localbob (local store wins)", r.name)
	}
	if r.domainName != "FILER01" {
		t.Errorf("domainName = %q, want FILER01 (machine domain for local account)", r.domainName)
	}
}

// TestResolveSID_MachineDomainGroup_DirectoryFallback is the group analog: a
// GROUP SID for an AD-only group falls back to the directory by gidNumber.
func TestResolveSID_MachineDomainGroup_DirectoryFallback(t *testing.T) {
	mapper := sid.NewSIDMapper(0, 0, 0)
	local := &mockResolver{groups: map[uint32]string{}}
	foreign := &mockForeignResolver{
		gidHits: map[uint32]reverseHit{10000: {name: "domain users", domain: "DITTOFS"}},
	}
	h := NewLSARPCHandler(mapper, local)
	h.SetForeignSIDResolver(foreign)

	r := h.resolveSID(mapper.GroupSID(10000))
	if r.name != "domain users" {
		t.Errorf("name = %q, want \"domain users\"", r.name)
	}
	if r.sidType != SidTypeGroup {
		t.Errorf("sidType = %d, want %d (Group)", r.sidType, SidTypeGroup)
	}
	if r.domainName != "DITTOFS" {
		t.Errorf("domainName = %q, want DITTOFS", r.domainName)
	}
}

// TestResolveSID_MachineDomainUser_BothMiss confirms that when neither the local
// store nor the directory knows the uid, the SID still resolves (never faults) —
// it shows the generic unix_user:N name under the machine domain.
func TestResolveSID_MachineDomainUser_BothMiss(t *testing.T) {
	mapper := sid.NewSIDMapper(0, 0, 0)
	h := NewLSARPCHandler(mapper, &mockResolver{users: map[uint32]string{}})
	h.SetForeignSIDResolver(&mockForeignResolver{uidHits: map[uint32]reverseHit{}})

	r := h.resolveSID(mapper.UserSID(7))
	if r.name != "unix_user:7" {
		t.Errorf("name = %q, want unix_user:7", r.name)
	}
	if r.sidType != SidTypeUser {
		t.Errorf("sidType = %d, want %d (User)", r.sidType, SidTypeUser)
	}
}

// =============================================================================
// Opnum 15 (LsarLookupSids) and opnum 76 (LsarLookupSids3) framing
// =============================================================================

// buildLookupSidsStubData builds a LookupSids request stub for the given opnum.
// Opnum 15/57 lead with a 20-byte policy handle; opnum 76 does not.
func buildLookupSidsStubData(opnum uint16, sids []*sid.SID) []byte {
	var buf bytes.Buffer
	if opnum == OpLsarLookupSids || opnum == OpLsarLookupSids2 {
		policyHandle := make([]byte, 20)
		policyHandle[0] = 0x01
		buf.Write(policyHandle)
	}
	appendUint32Buf(&buf, uint32(len(sids))) // Count
	appendUint32Buf(&buf, 0x00020000)        // pointer to array
	appendUint32Buf(&buf, uint32(len(sids))) // conformant max count
	for range sids {
		appendUint32Buf(&buf, 0x00020004) // SID pointer
	}
	for _, s := range sids {
		appendUint32Buf(&buf, uint32(s.SubAuthorityCount))
		sid.EncodeSID(&buf, s)
	}
	return buf.Bytes()
}

// TestLSARPC_LookupSids_Opnum15_RoundTrip verifies opnum 15 dispatches (no
// fault, no PROCNUM_OUT_OF_RANGE) and emits the NON-EX translated-name layout.
// Decoding with withFlags=false must yield well-formed, in-range fields; the
// well-known SID maps and the unknown SID does not.
func TestLSARPC_LookupSids_Opnum15_RoundTrip(t *testing.T) {
	mapper := sid.NewSIDMapper(0, 0, 0)
	h := NewLSARPCHandler(mapper, &mockResolver{users: map[uint32]string{5: "alice"}})

	sids := []*sid.SID{
		sid.WellKnownAdministrators,   // BUILTIN\Administrators
		mapper.UserSID(5),             // DITTOFS\alice
		sid.ParseSIDMust("S-1-99-42"), // unknown
	}
	stub := buildLookupSidsStubData(OpLsarLookupSids, sids)
	reqData := buildTestRequest(50, OpLsarLookupSids, stub)
	req, err := ParseRequest(reqData)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}

	response := h.HandleRequest(req)
	hdr, err := ParseHeader(response)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if hdr.PacketType == PDUFault {
		t.Fatal("opnum 15 faulted; smbcacls/rpcclient lookupsids must get a response")
	}

	res := parseLookupSidsResponse(t, response, false) // opnum 15 → non-EX layout
	if len(res.names) != 3 {
		t.Fatalf("translated names = %d, want 3", len(res.names))
	}
	wantTypes := []uint16{SidTypeAlias, SidTypeUser, SidTypeUnknown}
	for i, wt := range wantTypes {
		if res.names[i].sidType != wt {
			t.Errorf("name[%d].sidType = %d, want %d", i, res.names[i].sidType, wt)
		}
	}
	if res.status != statusSomeNotMapped {
		t.Errorf("status = 0x%08x, want STATUS_SOME_NOT_MAPPED", res.status)
	}
	if res.mappedCount != 2 {
		t.Errorf("mappedCount = %d, want 2", res.mappedCount)
	}
	// Every mapped domain index must be in range — the proof the non-EX layout
	// is internally consistent (a stray Flags word would shift these).
	for i, n := range res.names {
		if n.sidType == SidTypeUnknown {
			if n.domainIdx != -1 {
				t.Errorf("unmapped name[%d] domainIdx = %d, want -1", i, n.domainIdx)
			}
			continue
		}
		if n.domainIdx < 0 || int(n.domainIdx) >= len(res.domains) {
			t.Errorf("name[%d] domainIdx %d out of range (%d domains)", i, n.domainIdx, len(res.domains))
		}
	}
}

// TestLSARPC_LookupSids3_Opnum76_RoundTrip exercises opnum 76 (no policy handle
// on the wire). It must parse the SID array from offset 0 and emit the EX
// layout. A wrong leading-offset or wrong entry layout is exactly what produced
// NT_STATUS_ARRAY_BOUNDS_EXCEEDED (#1342).
func TestLSARPC_LookupSids3_Opnum76_RoundTrip(t *testing.T) {
	mapper := sid.NewSIDMapper(0, 0, 0)
	h := NewLSARPCHandler(mapper, &mockResolver{users: map[uint32]string{9: "bob"}})

	sids := []*sid.SID{
		mapper.UserSID(9),
		sid.WellKnownEveryone,
	}
	stub := buildLookupSidsStubData(OpLsarLookupSids3, sids)
	reqData := buildTestRequest(76, OpLsarLookupSids3, stub)
	req, err := ParseRequest(reqData)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}

	response := h.HandleRequest(req)
	hdr, err := ParseHeader(response)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if hdr.PacketType == PDUFault {
		t.Fatal("opnum 76 faulted")
	}

	res := parseLookupSidsResponse(t, response, true) // opnum 76 → EX layout
	if len(res.names) != 2 {
		t.Fatalf("translated names = %d, want 2 (offset-0 SID parse)", len(res.names))
	}
	if res.names[0].sidType != SidTypeUser {
		t.Errorf("name[0].sidType = %d, want %d (User)", res.names[0].sidType, SidTypeUser)
	}
	if res.names[1].sidType != SidTypeWellKnownGroup {
		t.Errorf("name[1].sidType = %d, want %d (WellKnownGroup)", res.names[1].sidType, SidTypeWellKnownGroup)
	}
	if res.status != statusSuccess {
		t.Errorf("status = 0x%08x, want success (all mapped)", res.status)
	}
	if res.mappedCount != 2 {
		t.Errorf("mappedCount = %d, want 2", res.mappedCount)
	}

	// The opnum-76 EX entry must link name[0] to its referenced domain via a
	// valid DomainIndex (default machine domain "DITTOFS"). A wrong index here
	// is what makes a client render a bare "bob" instead of "DITTOFS\bob" — the
	// EX-path domain-linkage regression behind #1342/#1343. (57 and 76 share the
	// builder, so this also covers the Windows Explorer opnum-57 path.)
	di := res.names[0].domainIdx
	if di < 0 || int(di) >= len(res.domains) {
		t.Fatalf("name[0] domainIdx %d out of range (domains=%v)", di, res.domains)
	}
	if res.domains[di] != "DITTOFS" {
		t.Errorf("name[0] -> domain %q, want DITTOFS", res.domains[di])
	}
}

// TestLSARPC_LookupSids_PerOpnumLayout_ByteDiff proves the per-opnum layout
// difference is real: for the SAME single mapped SID, the opnum-15 (non-EX)
// stub is exactly 4 bytes shorter than the opnum-57 (EX) stub — the missing
// Flags word. This is the framing fix for #1342.
func TestLSARPC_LookupSids_PerOpnumLayout_ByteDiff(t *testing.T) {
	mapper := sid.NewSIDMapper(0, 0, 0)
	h := NewLSARPCHandler(mapper, &mockResolver{users: map[uint32]string{3: "carol"}})

	resp15 := h.HandleRequest(mustParse(t, buildTestRequest(1, OpLsarLookupSids,
		buildLookupSidsStubData(OpLsarLookupSids, []*sid.SID{mapper.UserSID(3)}))))
	resp57 := h.HandleRequest(mustParse(t, buildTestRequest(2, OpLsarLookupSids2,
		buildLookupSidsStubData(OpLsarLookupSids2, []*sid.SID{mapper.UserSID(3)}))))

	if len(resp15) == 0 || len(resp57) == 0 {
		t.Fatal("empty response")
	}
	diff := len(resp57) - len(resp15)
	if diff != 4 {
		t.Errorf("opnum57 stub - opnum15 stub = %d bytes, want 4 (one Flags ULONG per mapped name)", diff)
	}
}

func mustParse(t *testing.T, reqData []byte) *Request {
	t.Helper()
	req, err := ParseRequest(reqData)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	return req
}

// TestLSARPC_LookupSids_MixedBatch_PerOpnum runs the same mapped/unmapped batch
// through all three opnums and asserts STATUS_SOME_NOT_MAPPED + the mapped count
// hold regardless of layout.
func TestLSARPC_LookupSids_MixedBatch_PerOpnum(t *testing.T) {
	mapper := sid.NewSIDMapper(0, 0, 0)
	h := NewLSARPCHandler(mapper, &mockResolver{users: map[uint32]string{2: "dave"}})

	sids := []*sid.SID{
		mapper.UserSID(2),            // mapped
		sid.ParseSIDMust("S-1-99-7"), // unmapped
		sid.WellKnownEveryone,        // mapped
	}

	cases := []struct {
		opnum     uint16
		withFlags bool
	}{
		{OpLsarLookupSids, false},
		{OpLsarLookupSids2, true},
		{OpLsarLookupSids3, true},
	}
	for _, c := range cases {
		stub := buildLookupSidsStubData(c.opnum, sids)
		req := mustParse(t, buildTestRequest(100, c.opnum, stub))
		response := h.HandleRequest(req)
		if hdr, _ := ParseHeader(response); hdr.PacketType == PDUFault {
			t.Fatalf("opnum %d faulted on mixed batch", c.opnum)
		}
		res := parseLookupSidsResponse(t, response, c.withFlags)
		if len(res.names) != 3 {
			t.Errorf("opnum %d: names = %d, want 3", c.opnum, len(res.names))
		}
		if res.status != statusSomeNotMapped {
			t.Errorf("opnum %d: status = 0x%08x, want STATUS_SOME_NOT_MAPPED", c.opnum, res.status)
		}
		if res.mappedCount != 2 {
			t.Errorf("opnum %d: mappedCount = %d, want 2", c.opnum, res.mappedCount)
		}
	}
}

// =============================================================================
// Golden byte-count assertions for RPC_UNICODE_STRING / NDR array length
//
// These lock the #1342 off-by-one: a resolved name of N characters must be
// framed with RPC_UNICODE_STRING Length == MaximumLength == 2*N (bytes, NO
// terminator) and a referenced conformant+varying UTF-16 array whose
// MaxCount == Offset+ActualCount == N (chars, NO trailing NUL). Samba's
// ndr_check_steal_array_length cross-checks ActualCount against Length/2; the
// old "+1 / +NUL" encoding made got==expected+1 ("got 8 expected 7") and
// rpcclient returned NT_STATUS_ARRAY_BOUNDS_EXCEEDED.
// =============================================================================

// ndrString captures the exact wire fields of one RPC_UNICODE_STRING plus the
// conformant+varying UTF-16 array it references, as decoded from a real
// buildLookupSidsResponse stub.
type ndrString struct {
	length    uint16 // RPC_UNICODE_STRING.Length (bytes)
	maxLength uint16 // RPC_UNICODE_STRING.MaximumLength (bytes)
	maxCount  uint32 // array MaxCount (chars)
	offset    uint32 // array Offset (chars)
	actual    uint32 // array ActualCount (chars)
	value     string // decoded name
	utf16Len  int    // raw UTF-16 byte count actually written (no padding)
}

// decodeLookupSidsStrings walks a buildLookupSidsResponse stub and returns the
// referenced-domain name strings and the translated-name strings with their
// exact RPC_UNICODE_STRING + array length fields. It is intentionally a precise,
// hand-rolled reader (not the lenient parseLookupSidsResponse) so the byte
// counts are asserted, not inferred. withFlags selects EX (57/76) vs non-EX (15).
func decodeLookupSidsStrings(t *testing.T, stub []byte, withFlags bool) (domains, names []ndrString) {
	t.Helper()
	off := 0
	u16 := func() uint16 { v := binary.LittleEndian.Uint16(stub[off : off+2]); off += 2; return v }
	u32 := func() uint32 { v := binary.LittleEndian.Uint32(stub[off : off+4]); off += 4; return v }

	// Read an un-terminated conformant+varying UTF-16 array, capturing fields.
	readArray := func() (maxCount, offset, actual uint32, value string, rawLen int) {
		maxCount = u32()
		offset = u32()
		actual = u32()
		rawLen = int(actual) * 2
		data := stub[off : off+rawLen]
		off += rawLen
		for off%4 != 0 {
			off++
		}
		var sb []rune
		for i := 0; i+1 < len(data); i += 2 {
			sb = append(sb, rune(binary.LittleEndian.Uint16(data[i:i+2])))
		}
		return maxCount, offset, actual, string(sb), rawLen
	}

	// --- Referenced domain list ---
	_ = u32() // list pointer
	domCount := u32()
	_ = u32() // array pointer
	_ = u32() // max entries
	type domFixed struct{ length, maxLength uint16 }
	var domFixeds []domFixed
	if domCount > 0 {
		_ = u32() // conformant max count
		for range domCount {
			length := u16()
			maxLength := u16()
			_ = u32() // string pointer
			_ = u32() // SID pointer
			domFixeds = append(domFixeds, domFixed{length, maxLength})
		}
		for _, d := range domFixeds {
			mc, o, a, v, raw := readArray()
			domains = append(domains, ndrString{d.length, d.maxLength, mc, o, a, v, raw})
		}
		for range domCount {
			skipNDRSID(t, stub, &off)
		}
	}

	// --- Translated names ---
	nameCount := u32()
	_ = u32() // array pointer
	type nameFixed struct{ length, maxLength uint16 }
	var nameFixeds []nameFixed
	if nameCount > 0 {
		_ = u32() // conformant max count
		for range nameCount {
			_ = u16() // Use
			_ = u16() // pad/reserved
			length := u16()
			maxLength := u16()
			_ = u32() // name pointer
			_ = u32() // domain index
			if withFlags {
				_ = u32() // flags (EX only)
			}
			nameFixeds = append(nameFixeds, nameFixed{length, maxLength})
		}
		for _, n := range nameFixeds {
			mc, o, a, v, raw := readArray()
			names = append(names, ndrString{n.length, n.maxLength, mc, o, a, v, raw})
		}
	}
	return domains, names
}

// assertExactName checks one ndrString against an expected name: Length and
// MaximumLength are 2*N bytes, the array MaxCount/Offset+ActualCount are N
// chars, and no NUL terminator was written.
func assertExactName(t *testing.T, label string, got ndrString, want string) {
	t.Helper()
	n := len(encodeUTF16LE(want)) / 2 // UTF-16 code units
	if got.value != want {
		t.Errorf("%s: name = %q, want %q", label, got.value, want)
	}
	if int(got.length) != 2*n {
		t.Errorf("%s: RPC_UNICODE_STRING.Length = %d, want %d (2*%d, no NUL)", label, got.length, 2*n, n)
	}
	if int(got.maxLength) != 2*n {
		t.Errorf("%s: RPC_UNICODE_STRING.MaximumLength = %d, want %d (== Length, no NUL)", label, got.maxLength, 2*n)
	}
	if int(got.maxCount) != n {
		t.Errorf("%s: array MaxCount = %d, want %d (chars, no NUL)", label, got.maxCount, n)
	}
	if got.offset != 0 {
		t.Errorf("%s: array Offset = %d, want 0", label, got.offset)
	}
	if int(got.actual) != n {
		t.Errorf("%s: array ActualCount = %d, want %d (chars, no NUL) — the #1342 off-by-one", label, got.actual, n)
	}
	if got.utf16Len != 2*n {
		t.Errorf("%s: wrote %d UTF-16 bytes, want %d (a trailing NUL would add 2)", label, got.utf16Len, 2*n)
	}
}

// TestLSARPC_TranslatedName_ExactLengths_AllOpnums is the regression lock for
// #1342. A 12-character username ("abcdefghijkl") under the 7-character machine
// domain ("DITTOFS") is resolved and the response stub is decoded with exact
// byte counts. For EACH of opnum 15 (non-EX), 57 and 76 (EX) it asserts the
// translated name frames as Length=24/MaxCount=12 and the domain as
// Length=14/MaxCount=7 — both with no NUL. These are precisely the "got N+1
// expected N" cases the live Samba client rejected (12->13, 7->8).
func TestLSARPC_TranslatedName_ExactLengths_AllOpnums(t *testing.T) {
	const userName = "abcdefghijkl" // 12 chars
	const domainName = "DITTOFS"    // 7 chars (machine domain default)

	mapper := sid.NewSIDMapper(0, 0, 0)

	cases := []struct {
		name      string
		opnum     uint16
		withFlags bool
	}{
		{"opnum15_nonEX", OpLsarLookupSids, false},
		{"opnum57_EX", OpLsarLookupSids2, true},
		{"opnum76_EX", OpLsarLookupSids3, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := NewLSARPCHandler(mapper, &mockResolver{users: map[uint32]string{1: userName}})
			stub := buildLookupSidsStubData(c.opnum, []*sid.SID{mapper.UserSID(1)})
			req := mustParse(t, buildTestRequest(1, c.opnum, stub))
			response := h.HandleRequest(req)
			if hdr, _ := ParseHeader(response); hdr.PacketType == PDUFault {
				t.Fatalf("opnum %d faulted", c.opnum)
			}
			if len(response) <= 24 {
				t.Fatalf("response too short: %d bytes", len(response))
			}
			domains, names := decodeLookupSidsStrings(t, response[24:], c.withFlags)

			if len(names) != 1 {
				t.Fatalf("translated names = %d, want 1", len(names))
			}
			assertExactName(t, "name[0]", names[0], userName)

			if len(domains) != 1 {
				t.Fatalf("referenced domains = %d, want 1", len(domains))
			}
			assertExactName(t, "domain[0]", domains[0], domainName)
		})
	}
}

// TestLookupSids_MultiDomain_NoReferentCollision guards against the NDR
// referent-ID collision that crashed Explorer: with ≥2 referenced domains (a
// LookupSids batch mixing a machine-domain owner with AD-domain grants), the
// translated-names array pointer must not reuse a domain entry's referent ID.
// A collision corrupts pointer resolution, the client fails the whole batch
// (raw SIDs), and Explorer can crash.
func TestLookupSids_MultiDomain_NoReferentCollision(t *testing.T) {
	domains := []domainEntry{
		{name: "DITTOFS", sid: sid.ParseSIDMust("S-1-5-21-1-2-3")},
		{name: "CUBBIT", sid: sid.ParseSIDMust("S-1-5-21-4-5-6")},
	}
	domainMap := map[string]int{"DITTOFS": 0, "CUBBIT": 1}
	resolved := []resolvedSID{
		{name: "admin", sidType: SidTypeUser, domainName: "DITTOFS", domainSID: domains[0].sid},
		{name: "alice", sidType: SidTypeUser, domainName: "CUBBIT", domainSID: domains[1].sid},
		{name: "Domain Admins", sidType: SidTypeGroup, domainName: "CUBBIT", domainSID: domains[1].sid},
	}

	h := &LSARPCHandler{}
	out := h.buildLookupSidsResponse(resolved, domains, domainMap, true, true)

	// The second domain's name referent is 0x00020010 (0x00020008 + 1*8). It must
	// appear exactly ONCE in the response — reused as the translated-names array
	// pointer, it would appear twice (the pre-fix collision).
	if n := countLEUint32(out, 0x00020010); n != 1 {
		t.Errorf("referent 0x00020010 appears %d times, want 1 (collision regressed)", n)
	}
	// The translated-names array pointer now lives in a disjoint range.
	if n := countLEUint32(out, 0x00040000); n != 1 {
		t.Errorf("translated-names array pointer 0x00040000 appears %d times, want 1", n)
	}
}

// countLEUint32 counts non-overlapping 4-byte-aligned little-endian occurrences
// of v in b.
func countLEUint32(b []byte, v uint32) int {
	n := 0
	for i := 0; i+4 <= len(b); i += 4 {
		if binary.LittleEndian.Uint32(b[i:i+4]) == v {
			n++
		}
	}
	return n
}

// TestLookupSids_ReferencedDomains_DeferredOrder validates that the referenced-
// domain deferred pointees are emitted in NDR appearance order (name0, sid0,
// name1, sid1) rather than grouped (name0, name1, sid0, sid1). With ≥2 domains
// the grouped order makes the client read domain1's name as domain0's SID,
// corrupting the parse (raw SIDs / Explorer crash). Asserts domain0's SID sits
// BETWEEN the two domain names in the wire bytes.
func TestLookupSids_ReferencedDomains_DeferredOrder(t *testing.T) {
	d0 := sid.ParseSIDMust("S-1-5-21-1-2-3")
	d1 := sid.ParseSIDMust("S-1-5-21-4-5-6")
	domains := []domainEntry{{name: "DITTOFS", sid: d0}, {name: "CUBBIT", sid: d1}}
	domainMap := map[string]int{"DITTOFS": 0, "CUBBIT": 1}
	resolved := []resolvedSID{
		{name: "admin", sidType: SidTypeUser, domainName: "DITTOFS", domainSID: d0},
		{name: "alice", sidType: SidTypeUser, domainName: "CUBBIT", domainSID: d1},
	}

	out := (&LSARPCHandler{}).buildLookupSidsResponse(resolved, domains, domainMap, true, true)

	var sidBuf bytes.Buffer
	sid.EncodeSID(&sidBuf, d0)
	iName0 := bytes.Index(out, encodeUTF16LE("DITTOFS"))
	iName1 := bytes.Index(out, encodeUTF16LE("CUBBIT"))
	iSid0 := bytes.Index(out, sidBuf.Bytes())
	if iName0 < 0 || iName1 < 0 || iSid0 < 0 {
		t.Fatalf("missing bytes: name0=%d name1=%d sid0=%d", iName0, iName1, iSid0)
	}
	if iName0 >= iSid0 || iSid0 >= iName1 {
		t.Errorf("referenced-domain deferred order wrong: name0@%d sid0@%d name1@%d; want name0 < sid0 < name1",
			iName0, iSid0, iName1)
	}
}
